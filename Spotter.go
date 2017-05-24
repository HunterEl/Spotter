package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"time"
)

type headers []string

func (this *headers) Set(value string) error {
	*this = append(*this, value)
	return nil
}

func (this headers) String() string {
	var buffer bytes.Buffer
	for _, value := range this {
		buffer.WriteString(value)
	}

	return buffer.String()
}

type Result struct {
	requests      int64
	success       int64
	networkFailed int64
	badFailed     int64
	// more variables to follow
}

type Output struct {
	category string
	respBody string
}

type ResultFile struct {
	Net  []string
	Bad  []string
	Succ []string
}

type Configuration struct {
	request      *http.Request
	client       *http.Client
	requests     int64
	resultBuffer chan *Output
}

var (
	requests       int64
	clients        int
	requestMethod  string
	requestBody    string
	requestHeaders headers
	displayVersion bool
	version        = "dev" // replace during make with -ldflags
	build          = "dev" // replace during make with -ldflags
)

func init() {
	flag.Int64Var(&requests, "n", 1, "Number of requests")
	flag.IntVar(&clients, "c", 1, "Number of workers")
	flag.StringVar(&requestMethod, "r", "GET", "HTTP Request Type")
	flag.StringVar(&requestBody, "d", "", "The Request Data")
	flag.Var(&requestHeaders, "h", "The Request Headers")
	flag.BoolVar(&displayVersion, "v", false, "Version")
	flag.Usage = usage
}

func main() {
	flag.Parse()

	if displayVersion {
		fmt.Printf("Version: %s\nBuild: %s\n", version, build)
		os.Exit(0)
	}

	args := flag.Args()
	if len(args) != 1 {
		flag.Usage()
		os.Exit(1)
	}

	urlDirty := args[0]
	urlClean := checkURL(urlDirty)

	httpRequest := createHttpRequest(requestMethod, requestBody, requestHeaders, urlClean)

	// This is where the magic happens...
	fmt.Printf("Starting Benchmark with %d clients and %d requests per client\n", clients, requests)

	start := time.Now()
	var barrier sync.WaitGroup
	results := make(map[int]*Result)
	sigChannel := make(chan os.Signal, 2)
	signal.Notify(sigChannel, os.Interrupt)

	go func() {
		_ = <-sigChannel
		// print
		os.Exit(0)
	}()

	// Set the number of CPUs if it's not set in the environment.
	goMaxProcs := os.Getenv("GOMAXPROCS")
	if goMaxProcs == "" {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	httpClient := &http.Client{
		Transport: transport,
	}

	bufferedChan := make(chan *Output, requests*int64(clients))

	config := &Configuration{
		httpRequest,
		httpClient,
		requests,
		bufferedChan,
	}

	barrier.Add(clients)
	for i := 0; i < clients; i++ {
		result := &Result{}
		results[i] = result
		go bench(config, result, &barrier)
	}

	total := 0
	netFailed := 0
	badFailed := 0
	succ := 0
	file := &ResultFile{}
	fmt.Printf("Waiting for %d clients to finish...\n", clients)
	barrier.Wait()
	elapsed := float64(time.Since(start).Seconds())
	close(bufferedChan)

	for output := range bufferedChan {
		switch output.category {
		case "net":
			netFailed++
			file.Net = append(file.Net, output.respBody)
		case "bad":
			badFailed++
			file.Bad = append(file.Bad, output.respBody)
		case "succ":
			succ++
			file.Succ = append(file.Succ, output.respBody)
		}
		total++
	}

	stats, err := json.Marshal(file)
	if err != nil {
		fmt.Println("ERROR MARSHALLING JSON: ", err)
	}

	err = ioutil.WriteFile("output.json", stats, 0644)
	if err != nil {
		fmt.Println("Couldn't write file: ", err)
	}

	fmt.Printf("\nRequest Number: %d\nSuccessful: %d\nNetwork Failed: %d\nBad Failed: %d\nRequests Per Second: %10f", total, succ, netFailed, badFailed, float64(total)/elapsed)
	fmt.Printf("\nProgram took: %10f second(s)\n", elapsed)
}

// Can Configure SSL and redirect policy later.
var transport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	MaxIdleConns:          100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

func bench(conf *Configuration, result *Result, barrier *sync.WaitGroup) {
	for result.requests < conf.requests {

		resp, err := conf.client.Do(conf.request)
		result.requests++
		if err != nil {
			conf.resultBuffer <- &Output{"net", err.Error()}
			continue
		}

		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Println("ERROR: ", err.Error())
		}

		statusCode := resp.StatusCode
		if statusCode == 200 {
			conf.resultBuffer <- &Output{"succ", string(bodyBytes)}
		} else {
			conf.resultBuffer <- &Output{"bad", string(bodyBytes)}
		}
	}

	// Treating like thread barrier in Java.
	barrier.Done()
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	flag.PrintDefaults()
}

func checkURL(uri string) *url.URL {
	if !strings.Contains(uri, "://") && !strings.HasPrefix(uri, "//") {
		uri = "//" + uri
	}

	url, error := url.Parse(uri)
	if error != nil {
		log.Fatalf("Could not parse url %q: %v", url, error)
	}

	if url.Scheme == "" {
		fmt.Println("Could not find URL scheme! Using HTTP.")
		url.Scheme = "http"
	}

	return url
}

func createHttpBody(body string) io.Reader {
	/// Body could be a file. E.G. -> "@relativePathToFile"
	if strings.HasPrefix(body, "@") {
		fileName := body[1:]
		file, err := os.Open(fileName)
		if err != nil {
			log.Fatalf("Could not read from File %s %v", fileName, err)
		}
		// os.File implments "Read" so it can be an io.Reader
		return file
	}
	return strings.NewReader(body)
}

func writeOutputFile(location string, body []byte) {
	if _, err := os.Stat(location); err != nil {
		fmt.Println("File %s Exists!", location)
		scanner := bufio.NewScanner(os.Stdin)
		var text string
		for {
			fmt.Println("Overwrite file? (y/n): ")
			scanner.Scan()
			text = scanner.Text()
			if strings.EqualFold(text, "n") {
				fmt.Println("Exiting!")
				os.Exit(1)
			} else if strings.EqualFold(text, "y") {
				ioutil.WriteFile(location, body, 0644)
			}
			// maybe allow q or other things to quit?
		}
	}
}

func extractHeaderKV(header string) (string, string) {
	splitHeader := strings.Split(header, ":")
	if len(splitHeader) != 2 {
		log.Fatalf("Malformed Request Header!\n%v", header)
	}

	return splitHeader[0], splitHeader[1]
}

func createHttpRequest(requestMethod string, requestBody string, requestHeaders headers, url *url.URL) *http.Request {
	req, err := http.NewRequest(requestMethod, url.String(), createHttpBody(requestBody))
	if err != nil {
		log.Fatalf("Couldn't create HTTP Request!\n%v", err)
	}

	for _, value := range requestHeaders {
		key, value := extractHeaderKV(value)
		req.Header.Add(key, value)
	}

	return req
}

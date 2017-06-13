package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
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
	requests        int64
	clients         int
	requestMethod   string
	requestBody     string
	outputFile      string
	requestHeaders  headers
	displayVersion  bool
	requestTimeout  time.Duration
	redirects       int
	devLogger       bool
	obnoxiousHeader bool
	version         = "dev" // replace during make with -ldflags
	build           = "dev" // replace during make with -ldflags
)

const (
	GunShow               = "\U0001f4aa"
	DefaultRequestTimeout = 30 * time.Second
	DefaultRedirects      = 10
	ObnoxiousHeader       = `
 ______     ______   ______     ______   ______   ______     ______
/\  ___\   /\  == \ /\  __ \   /\__  _\ /\__  _\ /\  ___\   /\  == \
\ \___  \  \ \  _-/ \ \ \/\ \  \/_/\ \/ \/_/\ \/ \ \  __\   \ \  __<
 \/\_____\  \ \_\    \ \_____\    \ \_\    \ \_\  \ \_____\  \ \_\ \_\
  \/_____/   \/_/     \/_____/     \/_/     \/_/   \/_____/   \/_/ /_/`
)

func init() {
	flag.Int64Var(&requests, "requests", 1, "Number of requests")
	flag.IntVar(&clients, "clients", 1, "Number of workers")
	flag.StringVar(&requestMethod, "type", "GET", "HTTP Request Type")
	flag.StringVar(&requestBody, "data", "", "The Request Data")
	flag.StringVar(&outputFile, "output", "", "The Output File Location")
	flag.Var(&requestHeaders, "header", "The Request Headers")
	flag.BoolVar(&displayVersion, "version", false, "Version")
	flag.DurationVar(&requestTimeout, "reqTimeout", DefaultRequestTimeout, "Timeout Per Request")
	flag.IntVar(&redirects, "redirects", DefaultRedirects, "Number of redirects to allow. -1 means no follow.")
	flag.BoolVar(&devLogger, "dev", false, "Logging internals for dev use")
	flag.BoolVar(&obnoxiousHeader, "oh", false, "Displays a reallllly obnoxious header.(Please don't use this)")
	flag.Usage = usage
}

func main() {
	flag.Parse()

	if displayVersion {
		fmt.Printf("[SPOTTER]:\nVersion: %s\nBuild: %s\n", version, build)
		os.Exit(0)
	}

	if obnoxiousHeader {
		fmt.Println(ObnoxiousHeader)
	}

	args := flag.Args()
	if len(args) != 1 {
		flag.Usage()
		os.Exit(1)
	}

	urlDirty := args[0]
	if !strings.Contains(urlDirty, "://") && !strings.HasPrefix(urlDirty, "//") {
		logMeUpFam("Adding // to input url")
		urlDirty = "//" + urlDirty
	}

	urlClean, err := url.Parse(urlDirty)
	if err != nil {
		fmt.Printf("[SPOTTER]: Could not parse URL %q: %v", urlDirty, err)
		os.Exit(1)
	}

	httpRequest := createHttpRequest(requestMethod, requestBody, requestHeaders, urlClean)

	fmt.Printf("[SPOTTER]: Starting tests with %d clients and %d requests per client\n", clients, requests)

	start := time.Now()
	var barrier sync.WaitGroup
	sigChannel := make(chan os.Signal, 2)
	signal.Notify(sigChannel, os.Interrupt)

	go func() {
		_ = <-sigChannel
		fmt.Println("[SPOTTER]: Exiting on interrupt...")
		os.Exit(0)
	}()

	maxProcs := os.Getenv("GOMAXPROCS")
	if maxProcs == "" {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	defaultLocalAddr := net.IPAddr{
		IP: net.IPv4zero,
	}

	defaultTLSConfig := &tls.Config{
		InsecureSkipVerify: true,
	}

	dialer := &net.Dialer{
		LocalAddr: &net.TCPAddr{IP: defaultLocalAddr.IP, Zone: defaultLocalAddr.Zone},
		KeepAlive: 30 * time.Second,
		Timeout:   requestTimeout,
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		Dial:  dialer.Dial,
		ResponseHeaderTimeout: requestTimeout,
		TLSClientConfig:       defaultTLSConfig,
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConnsPerHost:   10000, // this should be a variable :thinking_face:
	}

	httpClient := &http.Client{
		Transport: transport,
	}

	httpClient.CheckRedirect = func(req *http.Request, reqList []*http.Request) error {
		switch {
		case redirects == -1:
			return http.ErrUseLastResponse
		case len(reqList) > redirects:
			return fmt.Errorf("[SPOTTER]: Followed %d redirects. Stopping...", redirects)
		default:
			return nil
		}
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
		logMeUpFam(fmt.Sprintf("Starting client: %d", i))
		go bench(config, &barrier, i)
	}

	total := 0
	netFailed := 0
	badFailed := 0
	succ := 0
	file := &ResultFile{}

	fmt.Println("[SPOTTER]: Drum roll please...")
	logMeUpFam(fmt.Sprintf("Waiting for %d clients to finish...\n", clients))
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

	if outputFile != "" {
		stats, err := json.Marshal(file)
		if err != nil {
			fmt.Println("[SPOTTER]: Error Marshalling JSON: ", err)
		}
		err = writeOutputFile(outputFile, stats)
		if err != nil {
			fmt.Println("[SPOTTER]: Couldn't Write Output File: ", err)
		}
	}

	fmt.Println("RESULTS:")
	fmt.Printf("- Request Number: %d\n", total)
	fmt.Printf("- Successful: %d\n", succ)
	fmt.Printf("- Network Failed: %d\n", netFailed)
	fmt.Printf("- Bad Failed: %d\n", badFailed)
	fmt.Printf("- Requests Per Second: %10f\n", float64(total)/elapsed)
	fmt.Printf("- Program took: %10f second(s)\n", elapsed)
}

func flexItOut(hunkLevel int) {
	f := bufio.NewWriter(os.Stdout)
	defer f.Flush()
	var b []byte
	for i := 0; i < hunkLevel; i++ {
		b = append(b, GunShow...)
		b = append(b, ' ')
	}
	f.Write(b)
}

func bench(conf *Configuration, barrier *sync.WaitGroup, id int) {
	defer barrier.Done()
	for i := int64(1); i <= conf.requests; i++ {
		logMeUpFam(fmt.Sprintf("Client %d making request %d", id, i))
		conf.resultBuffer <- crunchRequest(conf)
	}
}

func logMeUpFam(logMsg string) {
	if devLogger {
		log.Println(logMsg)
	}
}

func crunchRequest(conf *Configuration) *Output {
	resp, err := conf.client.Do(conf.request)
	if err != nil {
		return &Output{"net", err.Error()}
	}

	defer resp.Body.Close()
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		logMeUpFam("Error reading body of the response!")
		return &Output{"bad", err.Error()}
	}

	statusCode := resp.StatusCode

	if statusCode >= 200 && statusCode < 300 {
		return &Output{"succ", string(bodyBytes)}
	} else {
		return &Output{"bad", string(bodyBytes)}
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	flag.PrintDefaults()
}

func createHttpBody(body string) io.Reader {
	if strings.HasPrefix(body, "@") {
		fileName := body[1:]
		file, err := os.Open(fileName)
		if err != nil {
			log.Fatalf("[SPOTTER]: Could not read from File %s %v", fileName, err)
		}
		// os.File implements "Read" so it can be an io.Reader
		return file
	}
	return strings.NewReader(body)
}

func writeOutputFile(location string, body []byte) error {
	_, err := os.Stat(location)
	if err == nil {
		fmt.Printf("\n[SPOTTER]: File %s Exists!\n", location)
		scanner := bufio.NewScanner(os.Stdin)
		var text string
		for {
			fmt.Print("[SPOTTER]: Overwrite file? (y/n): ")
			scanner.Scan()
			text = scanner.Text()
			if strings.EqualFold(text, "n") {
				fmt.Println("[SPOTTER]: Exiting since you are being difficult...")
				os.Exit(1)
			} else if strings.EqualFold(text, "y") {
				err := ioutil.WriteFile(location, body, 0644)
				return err
			}
		}
	} else {
		err := ioutil.WriteFile(location, body, 0644)
		return err
	}
}

func extractHeaderKV(header string) (string, string) {
	splitHeader := strings.Split(header, ":")
	if len(splitHeader) != 2 {
		log.Fatalf("[SPOTTER]: Malformed Request Header:\n%v", header)
	}

	return splitHeader[0], splitHeader[1]
}

func createHttpRequest(requestMethod string, requestBody string, requestHeaders headers, url *url.URL) *http.Request {
	req, err := http.NewRequest(requestMethod, url.String(), createHttpBody(requestBody))
	if err != nil {
		log.Fatalf("[SPOTTER]: Couldn't create HTTP Request:\n%v", err)
	}

	for _, value := range requestHeaders {
		key, value := extractHeaderKV(value)
		req.Header.Add(key, value)
	}

	return req
}

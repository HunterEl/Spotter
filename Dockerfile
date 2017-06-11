FROM golang

WORKDIR /myapp

ADD . /myapp/

RUN make

ENTRYPOINT ["./spotter"]


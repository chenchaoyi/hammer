# Hammer

Lightweight HTTP(s) Stress test tool in Go

## Usage:

```shell
  -auth string
        set authorization flag (oauth|none) (default "none")
  -debug
        set debug flag
  -host string
        server host address (default "api.mobile.walmart.com")
  -oauthkey string
        set oauth key (default "8e83a8372268")
  -oauthsecret string
        set oauth secret (default "24d643594f7cf03a52f5f6fe7c1b60dd")
  -profile string
        traffic profile
  -proxy string
        Set HTTP proxy (need to specify scheme. e.g. http://127.0.0.1:8888) (default "none")
  -rps int
        set Requst Per Second (default 100)
```

#### To run the test with profile file:

```shell
GOPATH=`pwd` go run hammer.go -profile profiles/httpbin.json -rps 1
```

#### To run the test with more logic:

Logic is defined in src/trafficprofiles/trafficprofile.go

```shell
GOPATH=`pwd` go run hammer.go -rps 1
```

#### To run test with Oauth:

```shell
GOPATH=`pwd` go run hammer.go -rps 1 -auth="oauth"
```

#### To enable debug:

```shell
GOPATH=`pwd` go run hammer.go -rps 1 -debug
```

#### To build Hammer for Mac OSX:

```shell
GOPATH=`pwd` CGO_ENABLED=0 go build -o hammer.prod.mac hammer.go
```

#### To update traffic profile:

You will have to update the trafficprofiles pkg source (this will be updated with more details, and subject to change)

The file is:
```shell
src/trafficprofiles/trafficprofile.go
```

#### To build Hammer for Linux:

You need to properly compile/update Go for Linux first:

`brew install go --HEAD --cross-compile-common`

To build binary for Linux
```shell
GOOS=linux GOARCH=amd64 GOPATH=`pwd` CGO_ENABLED=0 go build -o hammer.prod.linux hammer.go
```

## Files:

* hammer.go - the client hammer tool
* server.go - a lightweight server just for testing purpose for now


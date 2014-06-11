Hammer
=========
HTTP Stress test tool in Go   

Files:
======
hammer.go - the client hammer tool   
server.go - a lightweight server just for testing purpose   

To run the test with profile file:
============================
```shell
GOPATH=`pwd`:$GOPATH go run hammer.go -profile ${path_to_profile_file} -rps 1   
```

To run the test with more logic:
============================
Logic is defined in src/trafficprofiles/trafficprofile.go

```shell
GOPATH=`pwd`:$GOPATH go run hammer.go -rps 1   
```

To run test with Oauth:
=======================
```shell
GOPATH=`pwd`:$GOPATH go run hammer.go -rps 1 -auth="oauth"   
```

To enable debug:
================
```shell
GOPATH=`pwd`:$GOPATH go run hammer.go -rps 1 -debug   
```

To build Hammer for Linux:
==========================
You need to properly compile/update Go for Linux first:

`brew install go --HEAD --cross-compile-common`

To build binary for Linux
```shell
GOOS=linux GOARCH=amd64 GOPATH=`pwd`:$GOPATH CGO_ENABLED=0 go build -o hammer.prod.linux hammer.go
```

To update traffic profile:
==========================

You will have to update the trafficprofiles pkg source (this will be updated with more details, and subject to change)

The file is:
```shell
src/trafficprofiles/trafficprofile.go
```


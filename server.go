package main

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
)

func hello(res http.ResponseWriter, req *http.Request) {
	res.Header().Set(
		"Content-Type",
		"text/html",
	)
	io.WriteString(
		res,
		`<doctype html>
<html>
     <head>
           <title>Hello World</title>
     </head>
     <body>
           Hello World!
     </body>
</html>`,
	)
}

func hello_in_json(res http.ResponseWriter, req *http.Request) {
	res.Header().Set(
		"Content-Type",
		"text/json",
	)
	io.WriteString(
		res,
		`{"msg": "hello world"
    , timestamp: "nothing here"}`,
	)
}

func main() {
	// to init with half of the CPU worthy of threads
	fmt.Println(runtime.NumCPU())
	runtime.GOMAXPROCS(runtime.NumCPU())

	http.HandleFunc("/hello", hello)
	http.HandleFunc("/hello_in_json", hello_in_json)
	http.ListenAndServe(":9000", nil)
}

// Tiny HTTP server used as a local target when developing/testing hammer.
package main

import (
	"io"
	"log"
	"net/http"
)

func hello(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, `<!doctype html>
<html>
  <head><title>Hello World</title></head>
  <body>Hello World!</body>
</html>`)
}

func helloJSON(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"msg":"hello world"}`)
}

func main() {
	http.HandleFunc("/hello", hello)
	http.HandleFunc("/hello_in_json", helloJSON)
	log.Println("test server listening on :9000")
	log.Fatal(http.ListenAndServe(":9000", nil))
}

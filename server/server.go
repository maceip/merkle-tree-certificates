package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	"os/exec"
	"strings"

	"errors"
	"tideland.dev/go/wait"

	"github.com/gorilla/mux"
)

type ThrottledHandler struct {
	throttle *wait.Throttle
	handler  http.Handler
}

type Assertion struct {
	Ens string
	Pem string
}

func NewThrottledHandler(limit wait.Limit, handler http.Handler) http.Handler {
	return &ThrottledHandler{
		throttle: wait.NewThrottle(limit, 1),
		handler:  handler,
	}
}

func (h *ThrottledHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	evt := func() error {
		h.handler.ServeHTTP(w, r)
		return nil
	}
	h.throttle.Process(context.Background(), evt)
}

func InspectAssertion(w http.ResponseWriter, r *http.Request) {
	app := "mtc"
	arg0 := "inspect"
	arg1 := "assertion"
	arg2 := "ens-assertion"

	cmd := exec.Command(app, arg0, arg1, arg2)
	stdout, err := cmd.Output()

	if err != nil {
		fmt.Println(err.Error())
		return
	}

	w.Write([]byte(string(stdout)))
}
func CreateAssertion(w http.ResponseWriter, r *http.Request) {

	ct := r.Header.Get("Content-Type")
	if ct != "" {
		mediaType := strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
		if mediaType != "application/json" {
			msg := "Content-Type header is not application/json"
			http.Error(w, msg, http.StatusUnsupportedMediaType)
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1048576)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var p Assertion
	err := dec.Decode(&p)
	if err != nil {
		var syntaxError *json.SyntaxError
		var unmarshalTypeError *json.UnmarshalTypeError

		switch {

		case errors.As(err, &syntaxError):
			msg := fmt.Sprintf("Request body contains badly-formed JSON (at position %d)", syntaxError.Offset)
			http.Error(w, msg, http.StatusBadRequest)

		case errors.Is(err, io.ErrUnexpectedEOF):
			msg := fmt.Sprintf("Request body contains badly-formed JSON")
			http.Error(w, msg, http.StatusBadRequest)

		case errors.As(err, &unmarshalTypeError):
			msg := fmt.Sprintf("Request body contains an invalid value for the %q field (at position %d)", unmarshalTypeError.Field, unmarshalTypeError.Offset)
			http.Error(w, msg, http.StatusBadRequest)

		case strings.HasPrefix(err.Error(), "json: unknown field "):
			fieldName := strings.TrimPrefix(err.Error(), "json: unknown field ")
			msg := fmt.Sprintf("Request body contains unknown field %s", fieldName)
			http.Error(w, msg, http.StatusBadRequest)

		case errors.Is(err, io.EOF):
			msg := "Request body must not be empty"
			http.Error(w, msg, http.StatusBadRequest)

		case err.Error() == "http: request body too large":
			msg := "Request body must not be larger than 1MB"
			http.Error(w, msg, http.StatusRequestEntityTooLarge)

		default:
			log.Print(err.Error())
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
		return
	}

	err = dec.Decode(&struct{}{})
	if !errors.Is(err, io.EOF) {
		msg := "Request body must only contain a single JSON object"
		http.Error(w, msg, http.StatusBadRequest)
		return
	}

	f, err := os.CreateTemp("", "example")
	if err != nil {
		log.Fatal(err)
	}

	vars := mux.Vars(r)
	fmt.Println(vars["pem"])

	if _, err := f.Write([]byte(p.Pem)); err != nil {
		log.Fatal(err)
	}

	app := "mtc"
	arg0 := "new-assertion"
	arg1 := "--tls-pem"
	arg2 := f.Name()
	arg3 := "--ens"
	arg4 := vars["ens"]
	arg5 := "--ip4"
	arg6 := strings.Split(r.RemoteAddr, ":")[0]
	arg7 := "-o"
	arg8 := vars["ens"] + "-assertion"

	cmd := exec.Command(app, arg0, arg1, arg2, arg3, arg4, arg5, arg6, arg7, arg8)
	stdout, err := cmd.Output()

	if err != nil {
		fmt.Println(err.Error())
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Write([]byte(string(stdout)))
}
func CreateRoot(w http.ResponseWriter, r *http.Request) {
	app := "mtc"
	arg0 := "ca"
	arg1 := "new"
	arg2 := "--batch-duration"
	arg3 := "5m"
	arg4 := "--lifetime"
	arg5 := "1h"
	arg6 := "ens-pki"
	arg7 := "ca.login.limo/root"

	cmd := exec.Command(app, arg0, arg1, arg2, arg3, arg4, arg5, arg6, arg7)
	stdout, err := cmd.Output()

	if err != nil {
		fmt.Println(err.Error())
		return
	}

	w.Write([]byte(string(stdout)))
}

func main() {

	r := mux.NewRouter()
	r.HandleFunc("/newroot", NewThrottledHandler(5, http.HandlerFunc(CreateRoot)).ServeHTTP).Methods("POST")
	r.HandleFunc("/assertion/{ens}", NewThrottledHandler(5, http.HandlerFunc(CreateAssertion)).ServeHTTP).Methods("POST")
	r.HandleFunc("/assertion", NewThrottledHandler(5, http.HandlerFunc(InspectAssertion)).ServeHTTP).Methods("GET")
	log.Fatal(http.ListenAndServe(":4433", r))
}

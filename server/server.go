package main

import (
	"context"
"strings"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"tideland.dev/go/wait"

	"github.com/gorilla/mux"
)

type ThrottledHandler struct {
	throttle *wait.Throttle
	handler  http.Handler
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

	// Print the output
	w.Write([]byte(string(stdout)))
}
func CreateAssertion(w http.ResponseWriter, r *http.Request) {
	app := "mtc"
	arg0 := "new-assertion"
	arg1 := "--tls-pem"
	arg2 := "../fixture/user-domain-id.pub"
	arg3 := "--ens"
	arg4 := "maceip.eth"
	arg5 := "--ip4"
	arg6 := strings.Split(r.RemoteAddr,":")[0]
	arg7 := "-o"
	arg8 := "ens-assertion"

	cmd := exec.Command(app, arg0, arg1, arg2, arg3, arg4, arg5, arg6, arg7, arg8)
	_, err := cmd.Output()

	if err != nil {
		fmt.Println(err.Error())
		return
	}

	// Print the output
	w.Write([]byte(string(r.RemoteAddr)))
}
func CreateRoot(w http.ResponseWriter, r *http.Request) {
	app := "mtc"
	arg0 := "ca"
	arg1 := "new"
	arg2 := "--batch-duration"
	arg3 := "5m"
	arg4 := "--lifetime"
	arg5 := "1h"
	arg6 := "ens-merkle-pki"
	arg7 := "login.limo/root"

	cmd := exec.Command(app, arg0, arg1, arg2, arg3, arg4, arg5, arg6, arg7)
	stdout, err := cmd.Output()

	if err != nil {
		fmt.Println(err.Error())
		return
	}

	// Print the output
	w.Write([]byte(string(stdout)))
}

func main() {
	r := mux.NewRouter()
	r.HandleFunc("/newroot", NewThrottledHandler(5, http.HandlerFunc(CreateRoot)).ServeHTTP).Methods("POST")
	r.HandleFunc("assertion", NewThrottledHandler(5, http.HandlerFunc(CreateAssertion)).ServeHTTP).Methods("POST")
	r.HandleFunc("/assertion", NewThrottledHandler(5, http.HandlerFunc(InspectAssertion)).ServeHTTP).Methods("GET")
	log.Fatal(http.ListenAndServe(":4433", r))
}

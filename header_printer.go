package proxyd

import (
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/log"
)

func printHeader(direct string, r *http.Request) {
	log.Info("---- %s ----\n", direct)
	log.Info("%s %s %s\n", r.Method, r.URL.RequestURI(), r.Proto)
	log.Info("Host: %s\n", r.Host)

	for k, v := range r.Header {
		log.Info("%s: %s\n", k, strings.Join(v, ","))
	}
}

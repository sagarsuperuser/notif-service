package httpapi

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Server struct {
	Mux *http.ServeMux
}

func New() *Server {
	m := http.NewServeMux()
	m.Handle("/metrics", promhttp.Handler())
	return &Server{Mux: m}
}

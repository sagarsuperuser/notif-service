package httpserver

import "github.com/gorilla/mux"

type Server struct {
	Mux *mux.Router
}

func New() *Server {
	return &Server{Mux: mux.NewRouter()}
}

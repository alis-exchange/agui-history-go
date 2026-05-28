package jsonrpc

import (
	"net/http"
	"strings"
)

type corsConfig struct {
	allowOrigin  string
	allowMethods string
	allowHeaders string
}

func defaultCORSConfig() corsConfig {
	return corsConfig{
		allowOrigin:  "*",
		allowMethods: "POST, OPTIONS",
		allowHeaders: "Content-Type, Authorization, X-Alis-Forwarded-Authorization, X-Alis-User-Id, X-Alis-User-Email",
	}
}

func (c *corsConfig) writeHeaders(rw http.ResponseWriter) {
	rw.Header().Set("Access-Control-Allow-Origin", c.allowOrigin)
	rw.Header().Set("Access-Control-Allow-Methods", c.allowMethods)
	rw.Header().Set("Access-Control-Allow-Headers", c.allowHeaders)
}

// CORSOption configures CORS when passed to [WithCORS].
type CORSOption func(*corsConfig)

// CORSAllowOrigin sets Access-Control-Allow-Origin.
func CORSAllowOrigin(origin string) CORSOption {
	return func(c *corsConfig) { c.allowOrigin = origin }
}

// CORSAllowMethods sets Access-Control-Allow-Methods.
func CORSAllowMethods(methods ...string) CORSOption {
	return func(c *corsConfig) {
		if len(methods) > 0 {
			c.allowMethods = strings.Join(methods, ", ")
		}
	}
}

// CORSAllowHeaders sets Access-Control-Allow-Headers.
func CORSAllowHeaders(headers ...string) CORSOption {
	return func(c *corsConfig) {
		if len(headers) > 0 {
			c.allowHeaders = strings.Join(headers, ", ")
		}
	}
}

// WithCORS enables CORS for the JSON-RPC handler.
func WithCORS(opts ...CORSOption) JSONRPCHandlerOption {
	cfg := defaultCORSConfig()
	for _, o := range opts {
		o(&cfg)
	}
	return func(h *jsonrpcHandler) {
		c := cfg
		h.cors = &c
	}
}

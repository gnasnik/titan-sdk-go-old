package config

import (
	"net/http"
)

type TraversalMode int

const (
	// TraversalModeDFS only supports retrieving CAR files and auto decodes them into the raw file format.
	TraversalModeDFS TraversalMode = iota + 1
	// TraversalModeRange allows you to retrieve files of any type, but it does not decode, the files will be retrieved in their original format.
	// It's important to note that when using `TraversalModeRange` to retrieve a CAR file, the entire file must be downloaded before it can be decoded.
	TraversalModeRange
)

// Config is a set of titan SDK options.
type Config struct {
	Address    string
	Token      string
	HttpClient *http.Client
	Mode       TraversalMode
	Concurrent int
	ListenAddr string
}

// Option is a single titan sdk Config.
type Option func(opts *Config)

// DefaultOption returns a default set of options.
func DefaultOption() Config {
	return Config{
		Mode:       TraversalModeDFS,
		ListenAddr: ":8863",
	}
}

// AddressOption set titan server address
func AddressOption(address string) Option {
	return func(opts *Config) {
		opts.Address = address
	}
}

// TokenOption set titan server access token
func TokenOption(token string) Option {
	return func(opts *Config) {
		opts.Token = token
	}
}

// Http3ClientOption set HTTP/3 client, ONLY support HTTP/3 protocol
func Http3ClientOption(client *http.Client) Option {
	return func(opts *Config) {
		opts.HttpClient = client
	}
}

// TraversalModeOption set the download file traversal algorithm, default using DFS pre-order walk algorithm for Dag.
func TraversalModeOption(mode TraversalMode) Option {
	return func(opts *Config) {
		opts.Mode = mode
	}
}

// ListenAddressOption set the listen address for titan client, default is :8863
func ListenAddressOption(addr string) Option {
	return func(opts *Config) {
		opts.ListenAddr = addr
	}
}

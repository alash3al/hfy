package svc

import (
	"errors"
	"net/url"
)

func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid url")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, errors.New("must be an http(s) url")
	}
	if u.User != nil {
		return nil, errors.New("credentials in the url are not allowed")
	}
	return u, nil
}

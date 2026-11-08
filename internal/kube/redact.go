package kube

import (
	"errors"
	"net/url"
	"regexp"
)

var urlWithScheme = regexp.MustCompile(`https?://[^\s"'<>]+`)

func redactURLUserinfo(s string) string {
	return urlWithScheme.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil || u.User == nil {
			return raw
		}
		u.User = nil
		return u.String()
	})
}

func redactedError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(redactURLUserinfo(err.Error()))
}

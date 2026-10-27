//go:build !cgo

package main

import (
	"encoding/json"
	"errors"
)

// hostCall is unavailable without cgo; unit tests replace it with a fake.
var hostCall = func(method string, payload any) (json.RawMessage, error) {
	return nil, errors.New("host callbacks require a cgo build")
}

func main() {}

package server

import (
	"net/url"
	"os"
)

type urlT = url.URL

func readDir(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out, err
}

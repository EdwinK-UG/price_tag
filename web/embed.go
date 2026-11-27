// Package web embeds the browser apps into the server binary so a deployment
// is one file.
package web

import "embed"

//go:embed all:shopper all:admin all:vendor all:shared all:operator
var FS embed.FS

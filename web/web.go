// Package web holds the static files for the browser test page.
// go:embed bakes them into the server binary, so there is one file to deploy.
package web

import "embed"

//go:embed index.html
var Files embed.FS

// Package web holds the static files for the browser test page.
// The go:embed directive below bakes them into the server binary, so there
// is a single file to deploy.
package web

import "embed"

// Files is the test page, served at "/".
//
//go:embed index.html
var Files embed.FS

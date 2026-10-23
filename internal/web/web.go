// Package web holds the management page. The HTML, CSS, JavaScript and logo
// are kept in separate files for editing and served as one document.
package web

import (
	_ "embed"
	"encoding/base64"
	"strings"
	"sync"
)

var (
	//go:embed page.html
	pageHTML string
	//go:embed page.css
	pageCSS string
	//go:embed page.js
	pageJS string
	// logo.png is a 64 px copy of assets/logo.png, inlined because the
	// repository is private and its files cannot be loaded from GitHub.
	//go:embed logo.png
	logoPNG []byte

	pageOnce  sync.Once
	pageBytes []byte
)

// Page returns the management page with its CSS, JavaScript and logo inlined.
func Page() []byte {
	pageOnce.Do(func() {
		page := strings.Replace(pageHTML, "/*LOGO*/", base64.StdEncoding.EncodeToString(logoPNG), 1)
		page = strings.Replace(page, "/*CSS*/", pageCSS, 1)
		page = strings.Replace(page, "/*JS*/", pageJS, 1)
		pageBytes = []byte(page)
	})
	return pageBytes
}

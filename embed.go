// Package pager держит встроенный список источников, чтобы бинарник
// запускался без внешних файлов.
package pager

import _ "embed"

//go:embed sources.json
var DefaultSources []byte

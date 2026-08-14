package dashboard

import "embed"

// Files contains the independent dashboard frontend shipped with the binary.
// The source remains editable under internal/dashboard/static.
//
//go:embed static/*
var Files embed.FS

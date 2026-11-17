package main

import "github.com/iotku/CalibreFast/pkg/opf"

// Aliases for the OPF types, which live in pkg/opf, under the names the
// rest of the app has always used.
type (
	OPF           = opf.Package
	OPFMetadata   = opf.Metadata
	opfIdentifier = opf.Identifier
	opfMeta       = opf.Meta
)

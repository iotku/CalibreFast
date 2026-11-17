// Package imageutil makes covers and thumbnails from image bytes, and finds
// the picture that is a PDF's first page.
//
// Importing it registers decoders for JPEG, PNG, GIF, TIFF and WebP with
// package image. It works on bytes and paths only; caching and storage are
// the caller's.
package imageutil

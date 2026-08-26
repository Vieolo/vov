module github.com/vieolo/vov/examples/tasks

go 1.27

require github.com/vieolo/vov v0.0.0

// The example lives in the same repo but is its own module, so it consumes vov
// exactly as an external project would — through an import, across a module
// boundary. The replace points that import at the local source.
replace github.com/vieolo/vov => ../../

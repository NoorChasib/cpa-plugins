// Package deps pins the module's dependencies for go mod tidy. Its imports live in deps.go behind the
// deps build tag, so normal builds compile this empty package and link nothing.
package deps

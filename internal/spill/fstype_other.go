//go:build !linux

package spill

func inMemoryFS(string) bool { return false }

//go:build !linux && !windows

package main

func osCPUSeconds() float64 { return -1 }

# Changelog

## [Unreleased]
### Fixed
- Prevented `FireTower.read` from blocking indefinitely after the WebSocket tower is closed by watching both `readIn` and `closeChan`, returning `ErrorClose` as soon as shutdown begins.
- Ensured the gateway shutdown path logs and recycles any in-flight `FireInfo` while closing the tower, eliminating the goroutine leak reported in production.
- Kept the gateway build offline-friendly by stubbing required third-party modules via local replacements in `go.mod`.

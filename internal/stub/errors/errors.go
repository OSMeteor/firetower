package errors

import sterrors "errors"

// New proxies to the standard library errors.New helper to keep the existing
// call sites unchanged.
func New(text string) error {
	return sterrors.New(text)
}

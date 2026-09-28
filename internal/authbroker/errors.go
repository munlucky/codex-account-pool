package authbroker

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	ErrorNotLoggedIn            ErrorCode = "not_logged_in"
	ErrorReauthRequired         ErrorCode = "reauth_required"
	ErrorTemporarilyUnavailable ErrorCode = "temporarily_unavailable"
	ErrorInternal               ErrorCode = "internal_error"
)

type AuthError struct {
	Code ErrorCode
	Op   string
	Err  error
}

func (e *AuthError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		if e.Op == "" {
			return string(e.Code)
		}
		return fmt.Sprintf("%s: %s", e.Op, e.Code)
	}
	if e.Op == "" {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return fmt.Sprintf("%s: %s: %v", e.Op, e.Code, e.Err)
}

func (e *AuthError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func errorWithCode(code ErrorCode, op string, err error) error {
	return &AuthError{Code: code, Op: op, Err: err}
}

func Code(err error) ErrorCode {
	var authErr *AuthError
	if errors.As(err, &authErr) && authErr.Code != "" {
		return authErr.Code
	}
	return ErrorInternal
}

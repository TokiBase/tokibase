package rule

import (
	"errors"
	"fmt"
)

const (
	// MaxExprLen is the maximum accepted length (in bytes) of a filter/rule expression.
	MaxExprLen = 65536

	// MaxGroupDepth is the maximum accepted nesting depth of parenthesized groups.
	//
	// Upstream fexpr recurses per group without any bound, and a Go stack
	// overflow is a fatal, non-recoverable crash.
	MaxGroupDepth = 64
)

var (
	ErrExprTooLong = errors.New("filter expression is too long")
	ErrExprTooDeep = errors.New("filter expression is nested too deeply")
)

// CheckLimits rejects expressions that are longer than [MaxExprLen] or nested
// deeper than [MaxGroupDepth] before they reach the recursive parser.
//
// It is applied identically by the legacy compiler and by [Parse].
// Quoted text is skipped; parentheses in comments are counted (conservative).
func CheckLimits(expr string) error {
	if len(expr) > MaxExprLen {
		return fmt.Errorf("%w (max %d bytes)", ErrExprTooLong, MaxExprLen)
	}

	depth := 0
	var quote byte

	for i := 0; i < len(expr); i++ {
		c := expr[i]

		if quote != 0 {
			switch c {
			case '\\':
				i++ // skip the escaped char
			case quote:
				quote = 0
			}
			continue
		}

		switch c {
		case '\'', '"', '`':
			quote = c
		case '(':
			depth++
			if depth > MaxGroupDepth {
				return fmt.Errorf("%w (max %d groups)", ErrExprTooDeep, MaxGroupDepth)
			}
		case ')':
			if depth > 0 {
				depth--
			}
		}
	}

	return nil
}

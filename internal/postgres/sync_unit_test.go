package postgres

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsUndefinedTable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			"unrelated error",
			errors.New("connection refused"),
			false,
		},
		{
			"generic does not exist",
			errors.New(
				`column "foo" does not exist`,
			),
			false,
		},
		{
			"SQLSTATE 42P01",
			errors.New(
				`ERROR: relation "sessions" ` +
					`does not exist (SQLSTATE 42P01)`,
			),
			true,
		},
		{
			"bare SQLSTATE",
			errors.New("42P01"),
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isUndefinedTable(tt.err))
		})
	}
}

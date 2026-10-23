package gate

import "testing"

func TestMatchGlob(t *testing.T) {
	t.Parallel()

	tests := []struct {
		glob string
		name string
		want bool
	}{
		{"go/go.mod", "go/go.mod", true},
		{"go/go.mod", "go/sub/go.mod", false},
		{"go/db/migrations/**", "go/db/migrations/20260101_add.sql", true},
		{"go/db/migrations/**", "go/db/migrations/a/b.sql", true},
		{"go/db/migrations/**", "go/db/schema.sql", false},
		{"**/*_test.go", "main_test.go", true},
		{"**/*_test.go", "internal/pilot/gate/gate_test.go", true},
		{"**/*_test.go", "internal/pilot/gate/gate.go", false},
		{"rails/spec/**", "rails/spec/models/user_spec.rb", true},
		{"rails/spec/**", "rails/app/models/user.rb", false},
		{"go/**/*.sql.go", "go/internal/query/users.sql.go", true},
		{"go/**/**/*.sql.go", "go/users.sql.go", true},
		{"*.go", "a/b.go", false},
		{"[", "[", false},
	}

	for _, tt := range tests {
		t.Run(tt.glob+" "+tt.name, func(t *testing.T) {
			t.Parallel()

			if got := matchGlob(tt.glob, tt.name); got != tt.want {
				t.Errorf("matchGlob(%q, %q) = %v、期待値 = %v", tt.glob, tt.name, got, tt.want)
			}
		})
	}
}

package media

import "testing"

func TestKind(t *testing.T) {
	tests := []struct {
		k      Kind
		str    string
		sparse bool
	}{
		{Video, "video", true},
		{Audio, "audio", true},
		{Still, "still", false},
		{Unknown, "Kind(0)", false},
	}
	for _, tt := range tests {
		if got := tt.k.String(); got != tt.str {
			t.Errorf("%d.String() = %q, want %q", tt.k, got, tt.str)
		}
		if got := tt.k.UsesSparseID(); got != tt.sparse {
			t.Errorf("%s.UsesSparseID() = %v, want %v", tt.k, got, tt.sparse)
		}
	}
}

package storage

import (
	"testing"
)

func TestCompressedText(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"Empty", ""},
		{"Short", "hello world"},
		{"Long", "This is a much longer string that should definitely benefit from compression. Let's repeat it a few times. This is a much longer string that should definitely benefit from compression. Let's repeat it a few times. This is a much longer string that should definitely benefit from compression. Let's repeat it a few times."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := CompressedText(tt.text)

			// Test Value (Compression)
			value, err := ct.Value()
			if err != nil {
				t.Fatalf("Value() error = %v", err)
			}

			if tt.text == "" {
				empty, ok := value.([]byte)
				if !ok || len(empty) != 0 {
					t.Errorf("Expected empty []byte for empty text, got %T %v", value, value)
				}
				return
			}

			bytes, ok := value.([]byte)
			if !ok {
				t.Fatalf("Value() did not return []byte, got %T", value)
			}

			// Check magic header
			if string(bytes[:4]) != zstdMagic {
				t.Errorf("Expected zstd magic header, got %v", bytes[:4])
			}

			// Test Scan (Decompression)
			var scanned CompressedText
			err = scanned.Scan(bytes)
			if err != nil {
				t.Fatalf("Scan() error = %v", err)
			}

			if string(scanned) != tt.text {
				t.Errorf("Scan() result = %v, want %v", string(scanned), tt.text)
			}
		})
	}
}

func TestCompressedTextLegacy(t *testing.T) {
	// Test backward compatibility with uncompressed data
	legacyText := "plain old uncompressed text"
	var scanned CompressedText
	err := scanned.Scan([]byte(legacyText))
	if err != nil {
		t.Fatalf("Scan() legacy error = %v", err)
	}

	if string(scanned) != legacyText {
		t.Errorf("Scan() legacy result = %v, want %v", string(scanned), legacyText)
	}
}

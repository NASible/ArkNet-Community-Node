package main

import "testing"

func TestMediaStorageAllocationConfigGiB(t *testing.T) {
	for _, tt := range []struct {
		value   string
		want    int64
		invalid bool
	}{
		{"", 0, false}, {"0", 0, false}, {"1", 1 << 30, false}, {"10", 10 << 30, false},
		{"-1", -1, false}, {"-2", 0, true}, {"1GiB", 0, true}, {"1.5", 0, true},
		{"8589934591", 9223372035781033984, false}, {"8589934592", 0, true}, {"9223372036854775808", 0, true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			cfg, err := loadConfig(func(key string) string {
				if key == "ARKNET_MEDIA_STORAGE_ALLOCATION_GIB" {
					return tt.value
				}
				return ""
			})
			if (err != nil) != tt.invalid {
				t.Fatalf("error = %v, invalid = %v", err, tt.invalid)
			}
			if !tt.invalid && cfg.mediaStorageAllocation != tt.want {
				t.Fatalf("allocation = %d, want %d", cfg.mediaStorageAllocation, tt.want)
			}
		})
	}
}

func TestMediaStorageAllocationLegacyBytes(t *testing.T) {
	for _, value := range []string{"0", "8192", "-1"} {
		cfg, err := loadConfig(func(key string) string {
			if key == "ARKNET_MEDIA_STORAGE_ALLOCATION_BYTES" {
				return value
			}
			return ""
		})
		want := map[string]int64{"0": 0, "8192": 8192, "-1": -1}[value]
		if err != nil || cfg.mediaStorageAllocation != want {
			t.Fatalf("legacy %s: allocation=%d error=%v", value, cfg.mediaStorageAllocation, err)
		}
	}
	for _, value := range []string{"-2", "1GB", "9223372036854775808"} {
		if _, err := loadConfig(func(key string) string {
			if key == "ARKNET_MEDIA_STORAGE_ALLOCATION_BYTES" {
				return value
			}
			return ""
		}); err == nil {
			t.Fatalf("legacy invalid %q accepted", value)
		}
	}
}

func TestMediaStorageAllocationRejectsAmbiguousUnits(t *testing.T) {
	_, err := loadConfig(func(key string) string {
		if key == "ARKNET_MEDIA_STORAGE_ALLOCATION_GIB" || key == "ARKNET_MEDIA_STORAGE_ALLOCATION_BYTES" {
			return "1"
		}
		return ""
	})
	if err == nil {
		t.Fatal("both GiB and legacy bytes allocation were silently accepted")
	}
}

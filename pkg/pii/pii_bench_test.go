package pii

import "testing"

func BenchmarkMaskIP(b *testing.B) {
	input := "192.168.1.42"
	b.ReportAllocs()
	for b.Loop() {
		MaskIP(input)
	}
}

func BenchmarkMaskIPv6(b *testing.B) {
	input := "fe80:0000:0000:0000:0202:b3ff:fe1e:8329"
	b.ReportAllocs()
	for b.Loop() {
		MaskIP(input)
	}
}

func BenchmarkMaskEmail(b *testing.B) {
	input := "john.doe@example.com"
	b.ReportAllocs()
	for b.Loop() {
		MaskEmail(input)
	}
}

func BenchmarkMaskPhone(b *testing.B) {
	input := "+1 (555) 123-4567"
	b.ReportAllocs()
	for b.Loop() {
		MaskPhone(input)
	}
}

func BenchmarkMaskUserAgent(b *testing.B) {
	input := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	b.ReportAllocs()
	for b.Loop() {
		MaskUserAgent(input)
	}
}

func BenchmarkSanitize(b *testing.B) {
	input := "Error connecting to admin@corp.com at 10.0.0.1"
	b.ReportAllocs()
	for b.Loop() {
		Sanitize(input)
	}
}

func BenchmarkSanitizeDetail(b *testing.B) {
	input := "connection to db.prod.internal:5432 failed for user admin@corp.com"
	b.ReportAllocs()
	for b.Loop() {
		SanitizeDetail(input)
	}
}

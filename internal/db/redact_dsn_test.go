package db

import "testing"

func TestRedactDSN_HidesPassword(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{"postgres url", "postgres://app:s3cret@db:5432/app?sslmode=require", "postgres://app:***@db:5432/app?sslmode=require"},
		{"postgres password parameter", "postgres://app@db/app?password=s3cret&sslmode=require", "postgres://app@db/app?password=***&sslmode=require"},
		{"postgres sslpassword parameter", "postgres://app:s3cret@db/app?sslpassword=k3y", "postgres://app:***@db/app?sslpassword=***"},
		{"postgres keyword form", "host=db user=app password=s3cret dbname=app", "host=db user=app password=*** dbname=app"},
		{"postgres keyword form, quoted", "host=db password='s3 cret' dbname=app", "host=db password=*** dbname=app"},
		{"mysql url", "mysql://root:s3cret@tcp(db:3306)/app?parseTime=true", "mysql://root:***@tcp(db:3306)/app?parseTime=true"},
		{"mysql without a scheme", "root:s3cret@tcp(db:3306)/app", "root:***@tcp(db:3306)/app"},
		{"sqlserver url", "sqlserver://sa:s3cret@db:1433?database=app", "sqlserver://sa:***@db:1433?database=app"},
		{"sqlserver password parameter", "sqlserver://sa@db:1433?database=app&password=s3cret", "sqlserver://sa@db:1433?database=app&password=***"},
		{"password holding an at sign and a slash", "postgres://app:a@b/s3cret@db/app", "postgres://app:***@db/app"},
		{"password holding a question mark", "postgres://app:a?s3cret@db/app", "postgres://app:***@db/app"},
		{"at sign in a query value", "postgres://app:s3cret@db/app?application_name=me@host", "postgres://app:***@db/app?application_name=me@host"},
		{"no password", "postgres://app@db/app", "postgres://app@db/app"},
		{"no credentials", "postgres://db/app?sslmode=require", "postgres://db/app?sslmode=require"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RedactDSN(tt.dsn); got != tt.want {
				t.Errorf("RedactDSN(%q) = %q, want %q", tt.dsn, got, tt.want)
			}
		})
	}
}

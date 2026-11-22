package main

import (
	"strings"
	"testing"
)

// checkBaseURL refuses what could send a scenario to a database other than its own (a dbname / database /
// service / options key, a keyword/value DSN) before anything is built from it.
func TestCheckBaseURL(t *testing.T) {
	ok := []string{
		"postgres://u:p@localhost:5432/postgres?sslmode=disable",
		"postgresql://u:p@localhost:5432/postgres?sslmode=require&connect_timeout=5&application_name=x&sslrootcert=/a&sslcert=/b&sslkey=/c",
		"postgres://u@localhost/postgres",
	}
	for _, raw := range ok {
		if _, err := checkBaseURL(raw); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	bad := map[string]string{
		"postgres://u:p@localhost:5432/postgres?sslmode=disable&dbname=postgres": "dbname",
		"postgres://u:p@localhost:5432/postgres?database=postgres":               "database",
		"postgres://u:p@localhost:5432/postgres?service=svc":                     "service",
		"postgres://u:p@localhost:5432/postgres?options=-c%20search_path=x":      "options",
		"host=localhost user=u dbname=postgres":                                  "scheme",
		"mysql://u@localhost/x":                                                  "scheme",
	}
	for raw, want := range bad {
		_, err := checkBaseURL(raw)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it to mention %q", raw, err, want)
		}
	}
}

// urlFor keeps the rest of the URL and changes only the database.
func TestTemplateURLFor(t *testing.T) {
	base, err := checkBaseURL("postgres://u:p@localhost:5432/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	got, err := (&template{base: base}).urlFor("acc_x")
	if err != nil || got != "postgres://u:p@localhost:5432/acc_x?sslmode=disable" {
		t.Fatalf("urlFor = %q, %v", got, err)
	}
}

// A URL that does not parse is refused without echoing it: it carries the database password.
func TestCheckBaseURL_MalformedDoesNotEchoCredentials(t *testing.T) {
	const secret = "s3cr3t-pa55"
	_, err := checkBaseURL("postgres://postgres:" + secret + "%ZZ@127.0.0.1:1/postgres")
	if err == nil {
		t.Fatal("want an error for a bad escape")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error echoes the url: %v", err)
	}
	if !strings.Contains(err.Error(), "not a valid URL") {
		t.Fatalf("error does not say what is wrong: %v", err)
	}
}

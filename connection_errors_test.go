package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestConnectionFailureDiagnosticsKeepSecretsPrivate checks typed and wrapped
// connection-error categories while rejecting raw passwords, URLs, and server text.
func TestConnectionFailureDiagnosticsKeepSecretsPrivate(t *testing.T) {
	cases := []struct {
		name  string
		cause error
		want  string
	}{
		{"authentication", &pgconn.PgError{Code: "28P01", Message: "secret-password", Detail: "secret-password"}, "password authentication failed (SQLSTATE 28P01)"},
		{"access rules", &pgconn.PgError{Code: "28000", Message: "secret-password"}, "authorization rejected"},
		{"database", &pgconn.PgError{Code: "3D000", Message: "secret-password"}, "database does not exist"},
		{"capacity", &pgconn.PgError{Code: "53300", Message: "secret-password"}, "too many connections"},
		{"startup", &pgconn.PgError{Code: "57P03", Message: "secret-password"}, "not accepting connections yet"},
		{"unknown SQLSTATE", &pgconn.PgError{Code: "XX000", Message: "secret-password"}, "SQLSTATE XX000"},
		{"invalid SQLSTATE", &pgconn.PgError{Code: "secret-password", Message: "secret-password"}, "server rejected the connection"},
		{"DNS", &net.DNSError{Err: "secret-password", Name: "secret-password", Server: "secret-password"}, "hostname lookup failed"},
		{"certificate", &tls.CertificateVerificationError{Err: errors.New("secret-password")}, "TLS certificate verification failed"},
		{"hostname", x509.HostnameError{Host: "secret-password"}, "TLS certificate verification failed"},
		{"TLS", tls.RecordHeaderError{Msg: "secret-password"}, "TLS handshake failed"},
		{"refused", &net.OpError{Op: "secret-password", Net: "secret-password", Err: syscall.ECONNREFUSED}, "connection refused"},
		{"network timeout", &net.DNSError{IsTimeout: true, Err: "secret-password"}, "hostname lookup failed"},
		{"deadline", context.DeadlineExceeded, "connection timed out"},
		{"cancel", context.Canceled, "connection attempt canceled"},
		{"closed", io.EOF, "closed the connection during startup"},
		{"unknown", errors.New("secret-password"), "unclassified connection failure"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			// A driver can include a connection string in a wrapping error even when
			// its underlying typed cause is safe to classify.
			wrapped := fmt.Errorf("postgresql://user:secret-password@host/db: %w", test.cause)
			got := connectionFailure(wrapped).Error()
			if !strings.Contains(got, test.want) || strings.Contains(got, "secret-password") || strings.Contains(got, "postgresql://") {
				t.Fatalf("unexpected diagnostic: %s", got)
			}
		})
	}
}

// TestPostgresConnectionFailureDiagnostics checks safe SQLSTATE reporting for a
// nonexistent database using a real disposable PostgreSQL server.
func TestPostgresConnectionFailureDiagnostics(t *testing.T) {
	options, _, _ := testDatabase(t)
	dsn, err := getDSN(options)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := connect(context.Background(), dsn+" dbname=ddl_mt_missing_target")
	if connection != nil {
		connection.Close(context.Background())
		t.Fatal("unexpected connection to nonexistent database")
	}
	if err == nil || !strings.Contains(err.Error(), "requested database does not exist (SQLSTATE 3D000)") {
		t.Fatalf("expected typed PostgreSQL error, got %v", err)
	}
}

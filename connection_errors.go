package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
)

var sqlStatePattern = regexp.MustCompile(`^[A-Z0-9]{5}$`)

// connectionFailure returns a safe diagnostic built from a classified cause.
// Driver wrappers, server messages, DNS names, and certificate details are omitted
// because they may contain credentials.
func connectionFailure(err error) error {
	reason := connectionFailureReason(err)
	return fmt.Errorf("could not connect to database: %s (connection details omitted)", reason)
}

// connectionFailureReason maps typed and wrapped errors to fixed descriptions.
// Only a validated SQLSTATE may be copied from the original error; unknown causes
// remain intentionally generic.
func connectionFailureReason(err error) string {
	if errors.Is(err, context.Canceled) {
		return "connection attempt canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "connection timed out"
	}
	var server *pgconn.PgError
	if errors.As(err, &server) {
		reason := "server rejected the connection"
		switch server.Code {
		case "28P01":
			reason = "password authentication failed"
		case "28000":
			reason = "connection authorization rejected; check the role and server access rules"
		case "3D000":
			reason = "requested database does not exist"
		case "42501":
			reason = "server denied permission to connect"
		case "53300":
			reason = "server or connection pool has too many connections"
		case "57P03":
			reason = "server is not accepting connections yet"
		case "57P01":
			reason = "server is shutting down or terminated the connection"
		case "08P01":
			reason = "server reported a protocol error; check connection and startup settings"
		}
		if sqlStatePattern.MatchString(server.Code) {
			reason += " (SQLSTATE " + server.Code + ")"
		}
		return reason
	}
	var certVerification *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalidCertificate x509.CertificateInvalidError
	var roots x509.SystemRootsError
	if errors.As(err, &certVerification) || errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalidCertificate) || errors.As(err, &roots) {
		return "TLS certificate verification failed; check the hostname and trusted CA configuration"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "hostname lookup failed; check the hostname, DNS, and network access"
	}
	var network net.Error
	if pgconn.Timeout(err) || (errors.As(err, &network) && network.Timeout()) {
		return "connection timed out"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection refused; check the server and port"
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "server or connection pool closed the connection during startup"
	}
	var record tls.RecordHeaderError
	if errors.As(err, &record) {
		return "TLS handshake failed; check the endpoint and TLS settings"
	}
	var operation *net.OpError
	if errors.As(err, &operation) {
		return "network connection failed; check network access and the endpoint"
	}
	return "unclassified connection failure; check server or connection-pool logs"
}

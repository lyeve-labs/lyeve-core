package compliance

// DSARAuditWriter is the minimal interface for writing GDPR DSAR audit
// entries. An audit plugin implements it.
type DSARAuditWriter interface {
	LogEntry(action, resourceType, resourceID, ip, userAgent string, userID *string)
}

// DSARAuditWriterProvider is the shape a plugin implements to supply the
// writer. Exported so the plugin pins against the symbol the engine asserts.
type DSARAuditWriterProvider interface {
	DSARAuditWriter() DSARAuditWriter
}

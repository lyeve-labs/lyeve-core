# Emits every line that imports a package under -v prefix.
#
# Matching on the bare path is not enough: stack-trace fixtures and doc comments
# quote engine paths legitimately, and a plain grep reports those as violations.
# Only lines inside an import block, or a single-line import, count.
#
# Usage: awk -v prefix=github.com/... -f internal-import-scan.awk file.go

/^import[ \t]*\(/ { inblock = 1; next }
inblock && /^\)/   { inblock = 0; next }

inblock && index($0, "\"" prefix) { print FILENAME ":" FNR ":" $0; next }

/^import[ \t]/ && index($0, "\"" prefix) { print FILENAME ":" FNR ":" $0 }

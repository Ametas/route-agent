module github.com/Ametas/route-agent

go 1.24

// Agent certificates issued by the orchestrator before 2026-11-09 carry a random 8-byte serial
// without a cleared sign bit, so about half of them are negative. Go 1.23+ refuses to parse those
// ("x509: negative serial number"); Node/OpenSSL, which the TypeScript agent ran on, accepts them.
// Reissuing every node's certificate is not an option for an in-place switch, so the Go agent keeps
// accepting them. Covered by TestLoadTLSAcceptsNegativeSerial.
godebug x509negativeserial=1

require (
	golang.org/x/net v0.35.0
	google.golang.org/grpc v1.72.2
	google.golang.org/protobuf v1.36.6
)

require (
	golang.org/x/sys v0.30.0 // indirect
	golang.org/x/text v0.22.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250218202821-56aae31c358a // indirect
)

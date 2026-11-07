package binaries

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"google.golang.org/grpc"

	"github.com/Ametas/route-agent/internal/agentpb"
	"github.com/Ametas/route-agent/internal/locks"
	"github.com/Ametas/route-agent/internal/server/auth"
)

// UploadStream is the server side of every upload RPC.
type UploadStream = grpc.ClientStreamingServer[agentpb.BinaryChunkPayload, agentpb.UploadBinaryResponse]

// Per-RPC answers of spec C §0.2/§11.
const (
	msgNoData             = "No binary data received."
	msgNoValidBinary      = "No valid binary data received or unauthorized."
	msgNoValidSource      = "No valid source data received or unauthorized."
	prefixUploadFailed    = "Failed to upload binary: "
	prefixKernelFailed    = "Failed to install kernel module: "
	unknownVersion        = "unknown"
	errSizeLimitFormatStr = "upload exceeds the size limit of %d bytes"
)

// receiveSpec describes one upload RPC to the generic receiver.
type receiveSpec struct {
	rpc string
	// dir is the REAL directory the temp file is created in (created on the first data chunk).
	dir string
	// prefix starts the temp file name.
	prefix string
	// suffix ends it (".tar.gz" for the kernel source, ".tmp" otherwise).
	suffix   string
	maxBytes int64
	// unauthEmpty answers a stream that ended with zero chunks and a bad metadata secret.
	unauthEmpty string
	// empty answers an authorized stream that carried no bytes.
	empty string
	// failPrefix precedes an error text (receive or apply failure).
	failPrefix string
	// area is the area lock (internal/locks) the apply step holds; "" means none. It is shared
	// with the other groups' RPCs on the same area (ApplyConfig, ConfigureCaddy, ConfigureAwg…).
	area locks.Name
}

// received is the completely received upload handed to apply.
type received struct {
	// path is the real path of the temp file. apply may rename it away (the receiver then has
	// nothing to remove).
	path         string
	version      string // last non-empty version, "unknown" by default
	targetBinary string // last non-empty target_binary, "" by default
	bytes        int64
}

type applyFunc func(ctx context.Context, r received) (Result, error)

func answer(stream UploadStream, r Result) error {
	return stream.SendAndClose(&agentpb.UploadBinaryResponse{Success: r.Success, Message: r.Message})
}

// receive runs one upload: auth per chunk, stream to a temp file, then apply detached from the
// client stream (see the package doc). It answers with gRPC status OK in every authenticated or
// unauthenticated outcome; only a broken stream (client cancel before half-close) returns an error.
func (s *Service) receive(stream UploadStream, spec receiveSpec, apply applyFunc) error {
	ctx := stream.Context()
	gate := s.auth.Stream(ctx)
	version, target := unknownVersion, ""
	var f *os.File
	var n int64

	discard := func() {
		if f != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			f = nil
		}
	}

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			discard()
			s.log.Error("Error in "+spec.rpc+" stream", "err", err.Error())
			return err
		}
		if !gate.Admit(msg.GetOrchestratorSecret()) {
			discard()
			s.log.Warn("Unauthorized " + spec.rpc + " attempt rejected")
			return answer(stream, fail(auth.InvalidSecretMessage))
		}
		if v := msg.GetVersion(); v != "" {
			version = v
		}
		if t := msg.GetTargetBinary(); t != "" {
			target = t
		}
		chunk := msg.GetChunk()
		if len(chunk) == 0 {
			continue
		}
		if n+int64(len(chunk)) > spec.maxBytes {
			discard()
			s.log.Warn("Rejected "+spec.rpc+": upload exceeds the size limit", "limit", spec.maxBytes)
			return answer(stream, fail(spec.failPrefix+fmt.Sprintf(errSizeLimitFormatStr, spec.maxBytes)))
		}
		if f == nil {
			if f, err = createTemp(spec); err != nil {
				s.log.Error("Failed to create temp file for "+spec.rpc, "err", err.Error())
				return answer(stream, fail(spec.failPrefix+err.Error()))
			}
		}
		if _, err := f.Write(chunk); err != nil {
			discard()
			s.log.Error("Failed to write "+spec.rpc+" upload", "err", err.Error())
			return answer(stream, fail(spec.failPrefix+err.Error()))
		}
		n += int64(len(chunk))
	}

	if !gate.Verified() {
		// Only reachable with zero chunks (a chunk would have been admitted or rejected above).
		discard()
		s.log.Warn("Unauthorized " + spec.rpc + " attempt rejected")
		return answer(stream, fail(spec.unauthEmpty))
	}
	if n == 0 {
		discard()
		return answer(stream, fail(spec.empty))
	}
	if err := f.Sync(); err != nil {
		discard()
		return answer(stream, fail(spec.failPrefix+err.Error()))
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		f = nil
		_ = os.Remove(path)
		return answer(stream, fail(spec.failPrefix+err.Error()))
	}
	f = nil

	// The wait for the area (e.g. behind an ApplyConfig holding sing-box) is part of the apply and
	// detached from the client like the rest of it: once the half-close is in, the upload is
	// applied (spec C §12 item 34). Only a programming error (lock order) fails here.
	applyCtx := context.WithoutCancel(ctx)
	unlock := func() {}
	if spec.area != "" {
		var err error
		if applyCtx, unlock, err = s.locks.Lock(applyCtx, spec.area); err != nil {
			_ = os.Remove(path)
			s.log.Error("Failed to take the area lock", "rpc", spec.rpc, "err", err.Error())
			return answer(stream, fail(spec.failPrefix+err.Error()))
		}
	}
	var res Result
	func() {
		defer unlock()
		var err error
		res, err = apply(applyCtx, received{path: path, version: version, targetBinary: target, bytes: n})
		if err != nil {
			s.log.Error("Failed to apply uploaded file", "rpc", spec.rpc, "err", err.Error())
			res = fail(spec.failPrefix + err.Error())
		}
	}()
	// The answer must mean the temp file is gone (binaryReceiver.ts:127-140).
	_ = os.Remove(path)
	if err := answer(stream, res); err != nil {
		s.log.Warn("Could not deliver the "+spec.rpc+" answer (client gone?)", "err", err.Error(), "success", res.Success)
		return err
	}
	return nil
}

func createTemp(spec receiveSpec) (*os.File, error) {
	if err := os.MkdirAll(spec.dir, 0o755); err != nil {
		return nil, err
	}
	return os.CreateTemp(spec.dir, spec.prefix+"*"+spec.suffix)
}

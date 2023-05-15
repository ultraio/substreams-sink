package sink

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/streamingfast/derr"
	"io"
	"math"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/streamingfast/bstream"
	"github.com/streamingfast/dgrpc"
	"github.com/streamingfast/logging"
	"github.com/streamingfast/shutter"
	"github.com/streamingfast/substreams/client"
	"github.com/streamingfast/substreams/manifest"
	pbsubstreamsrpc "github.com/streamingfast/substreams/pb/sf/substreams/rpc/v2"
	pbsubstreams "github.com/streamingfast/substreams/pb/sf/substreams/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// IgnoreOutputModuleType can be used instead of the expected output module type
// when you want to validate this yourself, for example if you accept multiple
// output type(s).
const IgnoreOutputModuleType string = "@!##_IgnoreOutputModuleType_##!@"

// InferOutputModuleFromPackage can be used instead of the actual module's output name
// and has the effect that output module is extracted directly from the [pbsubstreams.Package]
// via the `SinkModule` field.
const InferOutputModuleFromPackage string = "@!##_InferOutputModuleFromSpkg_##!@"

type Sinker struct {
	*shutter.Shutter

	// Constructor (ordered)
	mode             SubstreamsMode
	pkg              *pbsubstreams.Package
	outputModule     *pbsubstreams.Module
	outputModuleHash string
	clientConfig     *client.SubstreamsClientConfig
	logger           *zap.Logger
	tracer           logging.Tracer

	// Options
	backOff         backoff.BackOff
	buffer          *blockDataBuffer
	blockRange      *bstream.Range
	infiniteRetry   bool
	finalBlocksOnly bool
	livenessChecker LivenessChecker

	// State
	stats *Stats
}

func New(
	mode SubstreamsMode,
	pkg *pbsubstreams.Package,
	outputModule *pbsubstreams.Module,
	hash manifest.ModuleHash,
	clientConfig *client.SubstreamsClientConfig,
	logger *zap.Logger,
	tracer logging.Tracer,
	opts ...Option,
) (*Sinker, error) {
	s := &Sinker{
		Shutter:          shutter.New(),
		clientConfig:     clientConfig,
		pkg:              pkg,
		outputModule:     outputModule,
		outputModuleHash: hex.EncodeToString(hash),
		mode:             mode,
		backOff:          backoff.NewExponentialBackOff(),
		stats:            newStats(logger),
		logger:           logger,
		tracer:           tracer,
	}

	for _, opt := range opts {
		opt(s)
	}

	if s.finalBlocksOnly && s.buffer != nil {
		s.logger.Debug("discarding undo buffer since final blocks only requested")
		s.buffer = nil
	}

	s.logger.Info("sinker configured",
		zap.Stringer("mode", s.mode),
		zap.Int("module_count", len(s.pkg.Modules.Modules)),
		zap.String("output_module_name", s.OutputModuleName()),
		zap.String("output_module_type", s.outputModule.Output.Type),
		zap.String("output_module_hash", s.outputModuleHash),
		zap.Stringer("client_config", (*substramsClientStringer)(s.clientConfig)),
		zap.Stringer("buffer", s.buffer),
		zap.Stringer("block_range", s.blockRange),
		zap.Bool("infinite_retry", s.infiniteRetry),
		zap.Bool("final_blocks_only", s.finalBlocksOnly),
		zap.Bool("liveness_checker", s.livenessChecker != nil),
	)

	return s, nil
}

type substramsClientStringer client.SubstreamsClientConfig

func (s *substramsClientStringer) String() string {
	config := (*client.SubstreamsClientConfig)(s)

	return fmt.Sprintf("%s (insecure: %t, plaintext: %t, JWT present: %t)", config.Endpoint(), config.Insecure(), config.PlainText(), config.JWT() != "")
}

func (s *Sinker) BlockRange() *bstream.Range {
	return s.blockRange
}

func (s *Sinker) Package() *pbsubstreams.Package {
	return s.pkg
}

func (s *Sinker) OutputModule() *pbsubstreams.Module {
	return s.outputModule
}

// OutputModuleHash returns the module output hash, can be used by consumer
// to warn if the module changed between restart of the process.
func (s *Sinker) OutputModuleHash() string {
	return s.outputModuleHash
}

func (s *Sinker) OutputModuleName() string {
	return s.outputModule.Name
}

// OutputModuleTypePrefixed returns the prefixed output module's type so the type
// will always be prefixed with "proto:".
func (s *Sinker) OutputModuleTypePrefixed() (prefixed string) {
	_, prefixed = sanitizeModuleType(s.outputModule.Output.Type)
	return
}

// OutputModuleTypeUnprefixed returns the unprefixed output module's type so the type
// will **never** be prefixed with "proto:".
func (s *Sinker) OutputModuleTypeUnprefixed() (unprefixed string) {
	unprefixed, _ = sanitizeModuleType(s.outputModule.Output.Type)
	return
}

// EndpointConfig returns the endpoint configuration used by this sinker instance.
func (s *Sinker) EndpointConfig() (endpoint string, plaintext bool, insecure bool) {
	return s.clientConfig.Endpoint(), s.clientConfig.PlainText(), s.clientConfig.Insecure()
}

// ApiToken returns the currently defined ApiToken sets on this sinker instance, ""
// is no api token was configured
func (s *Sinker) ApiToken() string {
	return s.clientConfig.JWT()
}

func (s *Sinker) Run(ctx context.Context, cursor *Cursor, handler SinkerHandler) {
	s.OnTerminating(func(_ error) {
		s.logger.Info("sinker terminating")
		s.stats.Close()
	})
	s.stats.OnTerminated(func(err error) { s.Shutdown(err) })

	logEach := 15 * time.Second
	if s.logger.Core().Enabled(zap.DebugLevel) {
		logEach = 5 * time.Second
	}

	s.stats.Start(logEach)

	fields := []zap.Field{zap.Duration("stats_refresh_each", logEach)}
	if cursor != nil {
		fields = append(fields, zap.Stringer("restarting_at", cursor.Block()))
	}
	if blockRange := s.adjustStreamRange(); blockRange != nil && blockRange.EndBlock() != nil {
		fields = append(fields, zap.String("end_at", fmt.Sprintf("#%d", (*blockRange.EndBlock())-1)))
	}

	s.logger.Info("starting sinker", fields...)
	lastCursor, err := s.run(ctx, cursor, handler)
	if err == nil {
		s.logger.Info("substreams ended correctly, reached your stop block", zap.Stringer("last_block_seen", lastCursor.Block()))
	}

	// If the context is canceled and we are here, it we have stop running without any other error, so Shutdown without error,
	// we are not the cause of the error. We still shutdown so Sinker last stats is still printed.
	shutdownErr := err
	if ctx.Err() == context.Canceled {
		shutdownErr = nil
	}

	s.Shutdown(shutdownErr)
}

func (s *Sinker) run(ctx context.Context, cursor *Cursor, handler SinkerHandler) (activeCursor *Cursor, err error) {
	activeCursor = cursor
	adjustedRange := s.adjustStreamRange()

	ssClient, closeFunc, callOpts, err := client.NewSubstreamsClient(s.clientConfig)
	if err != nil {
		return activeCursor, fmt.Errorf("new substreams client: %w", err)
	}
	s.OnTerminating(func(_ error) { closeFunc() })

	// We will wait at max approximatively 5m before dying
	backOff := s.backOff
	s.logger.Debug("configured default backoff", zap.String("back_off", fmt.Sprintf("%#v", backOff)))

	if !s.infiniteRetry {
		s.logger.Debug("configured backoff to stop after 15 retries")
		backOff = backoff.WithMaxRetries(backOff, 15)
	}

	backOff = backoff.WithContext(backOff, ctx)

	startBlock := uint64(0)
	if adjustedRange != nil {
		startBlock = adjustedRange.StartBlock()
	}

	var stopBlock uint64 = math.MaxUint64
	if adjustedRange != nil && adjustedRange.EndBlock() != nil {
		stopBlock = *adjustedRange.EndBlock()
	}

	for {
		req := &pbsubstreamsrpc.Request{
			StartBlockNum:   int64(startBlock),
			StopBlockNum:    stopBlock,
			StartCursor:     activeCursor.String(),
			FinalBlocksOnly: s.finalBlocksOnly,
			Modules:         s.pkg.Modules,
			OutputModule:    s.outputModule.Name,
			ProductionMode:  s.mode == SubstreamsModeProduction,
		}

		var receivedMessage bool
		activeCursor, receivedMessage, err = s.doRequest(ctx, activeCursor, req, ssClient, callOpts, handler)

		// If we received at least one message, we must reset the backoff
		if receivedMessage {
			backOff.Reset()
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				// We must assume that receiving an `io.EOF` means the stop block was reached. This is because
				// on network that can skips block number, it's possible that we requested to stop on a block
				// number that is no in the chain meaning we will receive `io.EOF` but the last seen block before
				// it is not our block number, we must have confidence in the Substreams provider to respect the
				// protocol
				return activeCursor, nil
			}

			// Retryable or not, we increment the error counter in all those cases
			SubstreamsErrorCount.Inc()

			var retryableError *derr.RetryableError
			if errors.As(err, &retryableError) {
				s.logger.Error("substreams encountered a retryable error", zap.Error(retryableError.Unwrap()))

				sleepFor := backOff.NextBackOff()
				if sleepFor == backoff.Stop {
					return activeCursor, ErrBackOffExpired
				}

				s.logger.Info("sleeping before re-connecting", zap.Duration("sleep", sleepFor))
				time.Sleep(sleepFor)
			} else {
				// Let's not wrap the error, it's not retryable to user will see directly his own error
				return activeCursor, err
			}
		}
	}
}

// adjusedStreamRange adjust the sinker range if defined and bounded
// when there is an undo buffer in use.
//
// When an undo buffer is used, we most finished +N block later than real
// stop block to ensure we accumulate enough blocks to assert "finality".
func (s *Sinker) adjustStreamRange() (out *bstream.Range) {
	if s.buffer != nil && s.blockRange != nil && s.blockRange.EndBlock() != nil {
		defer func() {
			s.logger.Debug("adjusted stream range", zap.Stringer("initial", s.blockRange), zap.Stringer("adjusted", out))
		}()

		return bstream.NewRangeExcludingEnd(
			s.blockRange.StartBlock(),
			(*s.blockRange.EndBlock())+uint64(s.buffer.Capacity()),
		)
	}

	return s.blockRange
}

func (s *Sinker) doRequest(
	ctx context.Context,
	activeCursor *Cursor,
	req *pbsubstreamsrpc.Request,
	ssClient pbsubstreamsrpc.StreamClient,
	callOpts []grpc.CallOption,
	handler SinkerHandler,
) (
	*Cursor,
	bool,
	error,
) {
	s.logger.Debug("launching substreams request", zap.Int64("start_block", req.StartBlockNum), zap.Stringer("cursor", activeCursor))
	receivedMessage := false

	stream, err := ssClient.Blocks(ctx, req, callOpts...)
	if err != nil {
		return activeCursor, receivedMessage, retryable(fmt.Errorf("call sf.substreams.rpc.v2.Stream/Blocks: %w", err))
	}

	for {
		if s.tracer.Enabled() {
			s.logger.Debug("substreams waiting to receive message", zap.Stringer("cursor", activeCursor))
		}

		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return activeCursor, receivedMessage, err
			}

			// Unauthanticated and canceled are not retryable
			if dgrpcError := dgrpc.AsGRPCError(err); dgrpcError != nil {
				switch dgrpcError.Code() {
				case codes.Unauthenticated, codes.Canceled:
					return activeCursor, receivedMessage, fmt.Errorf("stream failure: %w", err)
				}
			}

			return activeCursor, receivedMessage, retryable(err)
		}

		receivedMessage = true
		MessageSizeBytes.AddInt(proto.Size(resp))

		switch r := resp.Message.(type) {
		case *pbsubstreamsrpc.Response_Progress:
			for _, module := range r.Progress.Modules {
				ProgressMessageCount.Inc(module.Name)

				if processedRanges := module.GetProcessedRanges(); processedRanges != nil {
					latestEndBlock := uint64(0)
					for _, processedRange := range processedRanges.ProcessedRanges {
						if processedRange.EndBlock > latestEndBlock {
							latestEndBlock = processedRange.EndBlock
						}
					}

					ProgressMessageLastEndBlock.SetUint64(latestEndBlock, module.Name)
				}
			}

			if s.tracer.Enabled() {
				s.logger.Debug("received response Progress", zap.Reflect("progress", r))
			}

		case *pbsubstreamsrpc.Response_BlockScopedData:
			block := bstream.NewBlockRef(r.BlockScopedData.Clock.Id, r.BlockScopedData.Clock.Number)
			moduleOutput := r.BlockScopedData.Output

			if s.tracer.Enabled() {
				s.logger.Debug("received response BlockScopedData", zap.Stringer("at", block), zap.String("module_name", moduleOutput.Name), zap.Int("payload_bytes", len(moduleOutput.MapOutput.Value)))
			}

			// We record our stats before the buffer action, so user sees state of "stream" and not state of buffer
			s.stats.RecordBlock(block)
			HeadBlockNumber.SetUint64(block.Num())
			HeadBlockTimeDrift.SetBlockTime(r.BlockScopedData.Clock.Timestamp.AsTime())
			DataMessageCount.Inc()
			DataMessageSizeBytes.AddInt(proto.Size(r.BlockScopedData))
			BackprocessingCompletion.SetUint64(1)

			cursor, err := NewCursor(r.BlockScopedData.Cursor)
			if err != nil {
				return activeCursor, receivedMessage, fmt.Errorf("invalid received cursor, 'bstream' library in here is probably not up to date: %w", err)
			}

			activeCursor = cursor

			var dataToProcess []*pbsubstreamsrpc.BlockScopedData
			if s.buffer == nil {
				// No buffering, process directly
				dataToProcess = []*pbsubstreamsrpc.BlockScopedData{r.BlockScopedData}
			} else {
				dataToProcess, err = s.buffer.HandleBlockScopedData(r.BlockScopedData)
				if err != nil {
					return activeCursor, receivedMessage, fmt.Errorf("buffer add block data: %w", err)
				}
			}

			for _, blockScopedData := range dataToProcess {
				currentCursor, err := NewCursor(blockScopedData.Cursor)
				if err != nil {
					return activeCursor, receivedMessage, fmt.Errorf("invalid received cursor, 'bstream' library in here is probably not up to date: %w", err)
				}

				var isLive *bool
				if s.livenessChecker != nil {
					isLive = &blockNotLive
					if s.livenessChecker.IsLive(blockScopedData.Clock) {
						isLive = &liveBlock
					}
				}

				if err := handler.HandleBlockScopedData(ctx, blockScopedData, isLive, currentCursor); err != nil {
					return activeCursor, receivedMessage, fmt.Errorf("handle BlockScopedData message: %w", err)
				}
			}

		case *pbsubstreamsrpc.Response_BlockUndoSignal:
			undoSignal := r.BlockUndoSignal
			block := bstream.NewBlockRef(undoSignal.LastValidBlock.Id, undoSignal.LastValidBlock.Number)

			if s.tracer.Enabled() {
				s.logger.Debug("received response BlockUndoSignal", zap.Stringer("last_valid_block", block), zap.String("last_valid_cursor", undoSignal.LastValidCursor))
			}

			cursor, err := NewCursor(undoSignal.LastValidCursor)
			if err != nil {
				return activeCursor, receivedMessage, fmt.Errorf("invalid received cursor, 'bstream' library in here is probably not up to date: %w", err)
			}

			activeCursor = cursor

			// We record our stats before the buffer action, so user sees state of "stream" and not state of buffer
			s.stats.RecordBlock(block)
			UndoMessageCount.Inc()
			HeadBlockNumber.SetUint64(block.Num())
			// We don't have the block time in undo case for now, so we don't change it

			if s.buffer == nil {
				if err := handler.HandleBlockUndoSignal(ctx, r.BlockUndoSignal, activeCursor); err != nil {
					return activeCursor, receivedMessage, fmt.Errorf("handle BlockUndoSignal: %w", err)
				}
			} else {
				// In the case of dealing with an undo buffer, it's expected that a fork will never
				// go beyong the first block in the buffer because if it does, `s.buffer.HandleBlockUndoSignal` here
				// returns an error.
				//
				// This means ultimately that we expect to never call the downstream `BlockUndoSignalHandler` function.
				err = s.buffer.HandleBlockUndoSignal(r.BlockUndoSignal)
				if err != nil {
					return activeCursor, receivedMessage, fmt.Errorf("buffer undo block: %w", err)
				}
			}

		case *pbsubstreamsrpc.Response_DebugSnapshotData, *pbsubstreamsrpc.Response_DebugSnapshotComplete:
			s.logger.Warn("received debug snapshot message, there is no reason to receive those here", zap.Reflect("message", r))

		case *pbsubstreamsrpc.Response_Session:
			s.logger.Info("session initialized with remote endpoint", zap.String("trace_id", r.Session.TraceId))

		default:
			s.logger.Info("received unknown type of message", zap.Reflect("message", r))
			UnknownMessageCount.Inc()
		}
	}
}

func retryable(err error) error {
	return derr.NewRetryableError(err)
}

var (
	liveBlock    bool = true
	blockNotLive bool = false
)

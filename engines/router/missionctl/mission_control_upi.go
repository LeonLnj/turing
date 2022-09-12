package missionctl

import (
	"context"

	"github.com/caraml-dev/turing/engines/router/missionctl/errors"
	"github.com/caraml-dev/turing/engines/router/missionctl/fiberapi"
	"github.com/caraml-dev/turing/engines/router/missionctl/instrumentation/metrics"
	upiv1 "github.com/caraml-dev/universal-prediction-interface/gen/go/grpc/caraml/upi/v1"
	"github.com/gojek/fiber"
	fibergrpc "github.com/gojek/fiber/grpc"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type MissionControlUPI interface {
	Route(context.Context, fiber.Request) (*upiv1.PredictValuesResponse, *errors.TuringError)
}

type missionControlUpi struct {
	fiberRouter fiber.Component
}

// NewMissionControlUPI creates new instance of the MissingControl,
// based on the grpc configuration of fiber.yaml
func NewMissionControlUPI(
	cfgFilePath string,
	fiberDebugLog bool,
) (MissionControlUPI, error) {
	fiberRouter, err := fiberapi.CreateFiberRouterFromConfig(cfgFilePath, fiberDebugLog)
	if err != nil {
		return nil, err
	}

	return &missionControlUpi{
		fiberRouter: fiberRouter,
	}, nil
}

func (us *missionControlUpi) Route(
	ctx context.Context,
	fiberRequest fiber.Request) (
	*upiv1.PredictValuesResponse, *errors.TuringError) {
	var turingError *errors.TuringError
	defer metrics.GetMeasureDurationFunc(turingError, "route")()

	resp, ok := <-us.fiberRouter.Dispatch(ctx, fiberRequest).Iter()
	if !ok {
		turingError = errors.NewTuringError(
			errors.Newf(errors.BadResponse, "did not get back a valid response from the fiberHandler"), errors.GRPC,
		)
		return nil, turingError
	}
	if !resp.IsSuccess() {
		return nil, &errors.TuringError{
			Code:    resp.StatusCode(),
			Message: string(resp.Payload().([]byte)),
		}
	}

	grpcResponse, ok := resp.(*fibergrpc.Response)
	if !ok {
		turingError = errors.NewTuringError(
			errors.Newf(errors.BadResponse, "unable to parse fiber response into grpc response"), errors.GRPC,
		)
		return nil, turingError
	}

	var responseProto upiv1.PredictValuesResponse
	payloadByte, err := proto.Marshal(grpcResponse.Payload().(proto.Message))
	if err != nil {
		turingError = errors.NewTuringError(
			errors.Newf(errors.BadResponse, "unable to marshal payload"), errors.GRPC,
		)
		return nil, turingError
	}
	err = proto.Unmarshal(payloadByte, &responseProto)
	if err != nil {
		turingError = errors.NewTuringError(
			errors.Newf(errors.BadResponse, "unable to unmarshal into expected response proto"), errors.GRPC,
		)
		return nil, turingError
	}

	// attach metadata to context
	grpc.SendHeader(ctx, grpcResponse.Metadata)

	return &responseProto, nil
}

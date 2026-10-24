package cmd

import (
	"context"
	"errors"
	"io"

	"github.com/Songmu/ghalet"
)

func status(ctx context.Context, config string, failOnError bool, outStream io.Writer) error {
	result, err := ghalet.GetStatus(ctx, config)
	response := commandResponse{Status: result}
	var diagnostic ghalet.OperationError
	if !failOnError && result != nil && errors.As(err, &diagnostic) {
		response.Error = err.Error()
		err = nil
	}
	return printResponse(outStream, response, err)
}

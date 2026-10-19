package cmd

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/Songmu/ghalet"
)

type commandResponse struct {
	Status  *ghalet.Status           `json:"status,omitempty"`
	Results []ghalet.OperationResult `json:"results,omitempty"`
	Error   string                   `json:"error,omitempty"`
}

func printResponse(out io.Writer, response commandResponse, operationErr error) error {
	if operationErr != nil {
		var managementErr ghalet.OperationError
		if !errors.As(operationErr, &managementErr) {
			return operationErr
		}
		response.Error = operationErr.Error()
	}
	if err := json.NewEncoder(out).Encode(response); err != nil {
		return err
	}
	return operationErr
}

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"go-stock/internal/marketdataexport"
)

func main() {
	req, err := marketdataexport.DecodeRequest(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "market-data-export: %v\n", err)
		os.Exit(2)
	}
	envelope := marketdataexport.NewClient().Execute(context.Background(), req)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(envelope); err != nil {
		fmt.Fprintf(os.Stderr, "market-data-export: encode response: %v\n", err)
		os.Exit(3)
	}
}

#!/bin/bash
echo "--- RST (Relation Symbol Table) ---"
mkdir -p cmd/scip/rst
protoc --go_out=./cmd/scip/rst --go_opt=paths=source_relative -I. rst.proto
goimports -w ./cmd/scip/rst/rst.pb.go
go build ./cmd/scip

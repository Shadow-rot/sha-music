#!/bin/bash

# Telegram Userbot Runner Script

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}==================================${NC}"
echo -e "${GREEN}  Telegram Userbot Launcher${NC}"
echo -e "${GREEN}==================================${NC}"
echo ""

# Check if .env file exists
if [ -f .env ]; then
    echo -e "${GREEN}✓${NC} Loading environment variables from .env file..."
    export $(cat .env | grep -v '^#' | xargs)
else
    echo -e "${YELLOW}⚠${NC} No .env file found. Using system environment variables."
    echo -e "${YELLOW}⚠${NC} You can create a .env file from .env.example"
fi

# Check if required environment variables are set
if [ -z "$API_ID" ] || [ -z "$API_HASH" ] || [ -z "$PHONE" ]; then
    echo -e "${RED}✗${NC} Missing required environment variables!"
    echo ""
    echo "Please set the following:"
    echo "  - API_ID"
    echo "  - API_HASH"
    echo "  - PHONE"
    echo ""
    echo "You can either:"
    echo "  1. Create a .env file (see .env.example)"
    echo "  2. Export them in your shell:"
    echo "     export API_ID=\"your_api_id\""
    echo "     export API_HASH=\"your_api_hash\""
    echo "     export PHONE=\"+1234567890\""
    echo ""
    exit 1
fi

echo -e "${GREEN}✓${NC} Environment variables loaded"
echo -e "${GREEN}✓${NC} API_ID: ${API_ID}"
echo -e "${GREEN}✓${NC} PHONE: ${PHONE}"
echo ""

# Check if Go is installed
if ! command -v go &> /dev/null; then
    echo -e "${RED}✗${NC} Go is not installed!"
    echo "Please install Go from https://golang.org/dl/"
    exit 1
fi

echo -e "${GREEN}✓${NC} Go version: $(go version)"
echo ""

# Download dependencies if needed
if [ ! -d "vendor" ] && [ ! -f "go.sum" ]; then
    echo -e "${YELLOW}↓${NC} Downloading dependencies..."
    go mod download
    echo ""
fi

# Run the userbot
echo -e "${GREEN}▶${NC} Starting userbot..."
echo ""
go run userbot.go

APP_NAME=webrtc-sfu

all: build

build:
	@echo "Building $(APP_NAME)..."
	go build -o $(APP_NAME) main.go

run: build
	@echo "Running $(APP_NAME)..."
	./$(APP_NAME)

clean:
	@echo "Cleaning up..."
	rm -f $(APP_NAME)

.PHONY: all build run clean

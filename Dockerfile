# Use the official Golang image to build the binary
FROM golang:1.24-alpine as builder

# Set the working directory
WORKDIR /app

# Copy the go.mod and go.sum files
COPY go.mod go.sum ./

# Download dependencies (using vendor if you have it)
RUN go mod download

# Copy the rest of the source code
COPY . .

# Build the application
RUN go build -o main .

# Final stage: a tiny image to run the app
FROM alpine:latest
WORKDIR /root/
COPY --from=builder /app/main .

# The port your app runs on (usually 8080)
EXPOSE 8080

# Run the app
CMD ["./main"]
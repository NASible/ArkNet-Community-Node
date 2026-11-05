FROM golang:1.26-alpine AS build
WORKDIR /spike
COPY go.mod main.go main_test.go ./
RUN go test -v ./... && go vet ./...
RUN CGO_ENABLED=0 go build -trimpath -o /community-relay .
FROM scratch
COPY --from=build /community-relay /community-relay
USER 65532:65532
ENTRYPOINT ["/community-relay"]

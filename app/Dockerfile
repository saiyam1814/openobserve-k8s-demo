FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /checkout .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /checkout /checkout
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/checkout"]

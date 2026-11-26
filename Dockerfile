FROM golang:1.27.4-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /mbongo ./cmd/mbongo

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /mbongo /mbongo
EXPOSE 8080
ENTRYPOINT ["/mbongo"]

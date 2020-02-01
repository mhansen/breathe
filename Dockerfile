FROM golang:alpine as builder

RUN apk update && apk add git && apk add ca-certificates

WORKDIR /root
COPY go.mod go.sum *.go /root/
RUN go get -d -v
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go build -a breathe.go

FROM scratch
COPY --from=builder /root/breathe /root/
EXPOSE 9662
ENTRYPOINT ["/root/breathe"]

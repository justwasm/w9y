FROM btwiuse/arch:golang AS build

COPY . /app
WORKDIR /app
ENV CGO_ENABLED=0
RUN go mod tidy
RUN go build -o /bin/w9y ./cmd/w9y

CMD ["w9y"]

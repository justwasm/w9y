FROM btwiuse/arch:golang AS build

COPY . /app
WORKDIR /app
RUN go mod tidy
RUN go build -o /bin/w9y ./cmd/w9y

CMD ["/app/start.sh"]

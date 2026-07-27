## Start Agent
```bash
ulimit -n 999999
go build -o l7 ./tool
go run agent.go
```
## Start Control Server
```bash
go run control-server.go
```

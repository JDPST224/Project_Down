## Start Agent
```bash
ulimit -n 999999
go build l7.go
go run agent.go
```
## Start Control Server
```bash
go run control_server.go
```

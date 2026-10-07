// orchestrator consumes business commands and task results, plans and sequences NE tasks and runs sagas.
package main

import (
	"flag"

	"dxps/internal/orchestrator"
	"dxps/internal/svc"
)

func main() {
	instance := flag.String("instance", "", "static group member id (requires graceful shutdown; empty = dynamic)")
	stats := flag.String("stats", "127.0.0.1:9201", "loopback stats address")
	flag.Parse()
	ctx, cancel, cfg, log := svc.Boot("orchestrator")
	defer cancel()
	st := svc.OpenStore(ctx, cfg, log)
	defer st.Close()
	o, err := orchestrator.New(ctx, st, log)
	if err != nil {
		svc.Fatal(log, "init", err)
	}
	log.Info("running", "instance", *instance)
	if err := o.Run(ctx, orchestrator.ServiceConfig{Brokers: cfg.Kafka, Instance: *instance, StatsAddr: *stats}); err != nil {
		svc.Fatal(log, "run", err)
	}
}

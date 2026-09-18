package main

import (
	"fmt"
	"os"
	"runtime/pprof"
	"superdb/internal/engine"
)

func main() {
	f, err := os.Create(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := pprof.StartCPUProfile(f); err != nil {
		panic(err)
	}
	defer pprof.StopCPUProfile()

	db := engine.New()
	must(db, "CREATE TABLE users (id INT PRIMARY KEY, name TEXT, score INT)")
	for i := 0; i < 10000; i++ {
		must(db, fmt.Sprintf("INSERT INTO users VALUES (%d, 'user-%d', %d)", i, i, i%100))
	}
}

func must(db *engine.Database, sql string) {
	if _, err := db.Exec(sql); err != nil {
		panic(sql + ": " + err.Error())
	}
}

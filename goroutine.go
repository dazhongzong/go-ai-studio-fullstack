package main

import (
	"fmt"
	"sync"
)

func main() {
	chan1 := make(chan struct{})
	chan2 := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func(wg *sync.WaitGroup) {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			chan1 <- struct{}{}
			fmt.Println("A")
			<-chan2
		}

	}(&wg)
	go func(wg *sync.WaitGroup) {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			<-chan1
			fmt.Println("B")
			chan2 <- struct{}{}
		}
	}(&wg)
	wg.Wait()

}

package main

import (
	"fmt"

	"finally-main/internal/service"
)

func main() {
	feed := service.NewDataFeed("")
	fetch := feed.AnnouncementFetcher()
	anns, err := fetch("announcements-delistings", 4)
	fmt.Println("err =", err)
	fmt.Println("条数 =", len(anns))
	for i, a := range anns {
		if i >= 8 {
			break
		}
		fmt.Printf("  %d | %s | %s\n", a.PTime, a.AnnType, a.Title)
	}
}

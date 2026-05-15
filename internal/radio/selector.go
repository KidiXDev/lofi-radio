package radio

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func ChooseStation(stations []Station) Station {
	for i, station := range stations {
		fmt.Printf("%d. %s\n", i+1, station.Title)
	}

	reader := bufio.NewReader(os.Stdin)

	for {
		fmt.Print("Choose station: ")

		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		var choice int
		_, err := fmt.Sscanf(input, "%d", &choice)

		if err == nil && choice >= 1 && choice <= len(stations) {
			return stations[choice-1]
		}

		fmt.Println("Invalid choice, try again.")
	}
}

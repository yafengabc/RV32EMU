package main

import "unsafe"

// 基准算法：Leibniz 圆周率、冒泡排序、素数筛

var uart = (*uint32)(unsafe.Pointer(uintptr(0x60000000)))

func printChar(c byte) {
	*uart = uint32(c)
}

func piLeibniz(n int) uint32 {
	// Q16.16 定点
	sum := int32(0)
	sign := int32(0x10000)
	for k := 0; k < n; k++ {
		denom := int32(2*k + 1)
		term := int32(0x10000) / denom
		sum += sign * term
		sign = -sign
	}
	return uint32(sum * 4) // pi
}

func bubbleSort(arr *[16]uint8) int {
	for i := 0; i < 15; i++ {
		for j := 0; j < 15-i; j++ {
			if arr[j] > arr[j+1] {
				arr[j], arr[j+1] = arr[j+1], arr[j]
			}
		}
	}
	return int(arr[0])
}

func sieve(limit int) int {
	sieve := make([]bool, limit+1)
	for i := 2; i <= limit; i++ {
		sieve[i] = true
	}
	count := 0
	for i := 2; i <= limit; i++ {
		if sieve[i] {
			count++
			for j := i * i; j <= limit; j += i {
				sieve[j] = false
			}
		}
	}
	return count
}

func main() {
	// 跑多次以放大耗时
	for i := 0; i < 50; i++ {
		piLeibniz(20000)
	}
	arr := [16]uint8{5, 3, 8, 1, 9, 2, 7, 4, 0, 6, 15, 11, 13, 10, 14, 12}
	for i := 0; i < 1000; i++ {
		bubbleSort(&arr)
	}
	for i := 0; i < 100; i++ {
		sieve(1000)
	}
	// 停机标志：写一个特殊值到 UART
	printChar('D')
	printChar('O')
	printChar('N')
	E := byte(0)
	for {
		_ = E
	}
}

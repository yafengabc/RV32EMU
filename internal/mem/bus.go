package mem

// Bus 是统一访存接口：所有 load/store / 取指都经过它，
// 由 Bus 负责地址译码到 RAM/ROM/外设。size ∈ {1,2,4}。
type Bus interface {
	Read(addr uint32, size uint32) uint32
	Write(addr uint32, size uint32, val uint32)
}

// Peripheral 是被总线挂接的 MMIO 设备。offset 为相对设备基址的偏移。
type Peripheral interface {
	Name() string
	Read(offset uint32, size uint32) uint32
	Write(offset uint32, size uint32, val uint32)
}

// DeviceRange 描述一个挂在总线上的外设：基址 + 大小。
type DeviceRange struct {
	Base uint32
	Size uint32
	Dev  Peripheral
}

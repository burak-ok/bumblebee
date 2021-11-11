package loader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	hashstructure "github.com/mitchellh/hashstructure/v2"
	"github.com/pterm/pterm"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/global"
	"golang.org/x/sync/errgroup"
)

type LoadOptions struct {
	EbpfProg io.ReaderAt
}

type Loader interface {
	Load(ctx context.Context, opts *LoadOptions) error
}

func NewLoader(decoderFactory DecoderFactory) Loader {
	initMeter()
	return &loader{
		decoderFactory: decoderFactory,
	}
}

type loader struct {
	decoderFactory DecoderFactory
}

func (l *loader) Load(ctx context.Context, opts *LoadOptions) error {

	loaderProgress, _ := pterm.DefaultSpinner.Start("Loading BPF program and maps into Kernel")
	// Generate the spec from out eBPF elf file
	spec, err := ebpf.LoadCollectionSpecFromReader(opts.EbpfProg)
	if err != nil {
		loaderProgress.Fail()
		return err
	}

	btfMapMap := make(map[string]*btf.Map)

	// TODO: Delete Hack if possible
	for name, mapSpec := range spec.Maps {
		if mapSpec.Type == ebpf.RingBuf || mapSpec.Type == ebpf.PerfEventArray {
			btfMapMap[name] = mapSpec.BTF
			mapSpec.BTF = nil
			mapSpec.ValueSize = 0
		}
	}

	// Load our eBPF spec into the kernel
	coll, err := ebpf.NewCollection(spec)
	if err != nil {
		loaderProgress.Fail()
		return err
	}
	defer coll.Close()
	loaderProgress.Success()

	linkerProgress, _ := pterm.DefaultSpinner.Start("Linking BPF functions to associated probe/tracepoint")
	// For each program, add kprope/tracepoint
	for name, prog := range spec.Programs {
		switch prog.Type {
		case ebpf.Kprobe:
			var kp link.Link
			var err error
			if strings.HasPrefix(prog.SectionName, "kretprobe/") {
				kp, err = link.Kretprobe(prog.AttachTo, coll.Programs[name])
				if err != nil {
					linkerProgress.Fail()
					return fmt.Errorf("error attaching kretprobe '%v': %w", prog.Name, err)
				}
			} else {
				kp, err = link.Kprobe(prog.AttachTo, coll.Programs[name])
				if err != nil {
					linkerProgress.Fail()
					return fmt.Errorf("error attaching kprobe '%v': %w", prog.Name, err)
				}
			}
			defer kp.Close()
		default:
			linkerProgress.Fail()
			return errors.New("only kprobe programs supported")
		}
	}
	linkerProgress.Success()

	pterm.Info.Println("Starting map watches")

	eg, ctx := errgroup.WithContext(ctx)

	for name, bpfMap := range spec.Maps {
		name := name
		bpfMap := bpfMap
		if !shouldProcessMap(bpfMap) {
			continue
		}
		switch bpfMap.Type {
		case ebpf.PerfEventArray:
			fallthrough
		case ebpf.RingBuf:
			eg.Go(func() error {
				return l.startRingBuf(ctx, btfMapMap, coll, name)
			})
		case ebpf.Hash:
			eg.Go(func() error {
				return l.startHashMap(ctx, bpfMap, coll.Maps[name], name)
			})
		case ebpf.Array:
			eg.Go(func() error {
				return l.startHashMap(ctx, bpfMap, coll.Maps[name], name)
			})
		default:
			// TODO: Support more map types
			return errors.New("only ringbuf, and perf event array supported")
		}
	}

	return eg.Wait()
}

// Checks the given MapSpec to see if this map should be watched by our loader
// Will return true for maps that should be watched
func shouldProcessMap(mapSpec *ebpf.MapSpec) bool {
	secName := mapSpec.SectionName
	if strings.HasSuffix(secName, "counter") || strings.HasSuffix(secName, "gauge") || strings.HasSuffix(secName, "print") {
		return true
	}
	return false
}

func (l *loader) startRingBuf(
	ctx context.Context,
	btfMapMap map[string]*btf.Map,
	coll *ebpf.Collection,
	name string,
) error {
	// Initialize decoder
	d := l.decoderFactory()

	// TODO: Support *btf.Union
	t := btfMapMap[name].Value.(*btf.Struct)

	// Open a ringbuf reader from userspace RINGBUF map described in the
	// eBPF C program.
	rd, err := ringbuf.NewReader(coll.Maps[name])
	if err != nil {
		return fmt.Errorf("opening ringbuf reader: %v", err)
	}
	defer rd.Close()
	// Close the reader when the process receives a signal, which will exit
	// the read loop.
	go func() {
		<-ctx.Done()

		if err := rd.Close(); err != nil {
			pterm.Warning.Printf("closing ringbuf reader: %s", err)
		}
	}()

	for {
		record, err := rd.Read()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				return nil
			}
			pterm.Warning.Printf("reading from reader: %s", err)
			continue
		}
		result, err := d.DecodeBtfBinary(ctx, t, record.RawSample)
		if err != nil {
			return err
		}

		// TODO: Handle statistic, or structured logging
		byt, err := json.Marshal(result)
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s\n", name, byt)

	}
}

func (l *loader) startHashMap(
	ctx context.Context,
	mapSpec *ebpf.MapSpec,
	liveMap *ebpf.Map,
	name string,
) error {

	meter := global.Meter(ebpfMeter)
	d := l.decoderFactory()

	// Read loop reporting the total amount of times the kernel
	// function was entered, once per second.
	ticker := time.NewTicker(1 * time.Second)

	counter := metric.Must(meter).NewInt64Counter(name)
	valueMap := map[uint64]uint64{}
	for {
		select {
		case <-ticker.C:
			// var all_cpu_value []uint64
			mapIter := liveMap.Iterate()
			for {
				// Use generic key,value so we can decode ourselves
				var (
					key, value []byte
				)
				// log.Println("Checking Iterator..")
				if !mapIter.Next(&key, &value) {
					break
				}
				if err := mapIter.Err(); err != nil {
					return err
				}
				decodedKey, err := d.DecodeBtfBinary(ctx, mapSpec.BTF.Key, key)
				if err != nil {
					return err
				}

				labels := []attribute.KeyValue{}
				keyMap := map[string]string{}
				for k, v := range decodedKey {
					valAsStr := fmt.Sprint(v)
					thisKv := attribute.String(k, valAsStr)
					labels = append(labels, thisKv)
					keyMap[k] = valAsStr
				}
				keyHash, err := hashstructure.Hash(keyMap, hashstructure.FormatV2, nil)
				if err != nil {
					return err
				}

				decodedValue, err := d.DecodeBtfBinary(ctx, mapSpec.BTF.Value, value)
				if err != nil {
					return err
				}

				if len(decodedValue) > 1 {
					log.Fatal("only 1 value allowed")
				}
				intVal, ok := decodedValue[""].(uint64)
				if !ok {
					log.Fatal("only uint64 allowed")
				}

				oldVal := valueMap[keyHash]
				diff := intVal - oldVal
				if oldVal == intVal {
					continue
				}

				fmt.Printf("key: '%v' => val: %v\n", keyMap, intVal)
				valueMap[keyHash] = intVal
				meter.RecordBatch(ctx, labels, counter.Measurement(int64(diff)))
			}
		case <-ctx.Done():
			return nil
		}
	}
}

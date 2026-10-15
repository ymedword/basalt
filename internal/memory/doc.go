// Package memory 承载记忆系统：三档载体（markdown / sqlite / mem0）同语义。
// Package memory holds the memory system: three interchangeable backends
// (markdown / sqlite / mem0) with identical semantics.
//
// 骨架占位：实现随 US-6 落位。换后端 = 改一行配置（memory.backend），
// 作用域与分区语义三档一致——同一组场景断言在三档载体上都要跑通。
//
// Skeleton placeholder; the implementation lands with US-6. Switching backends
// is a one-line configuration change, and the same scenario assertions must
// hold on every backend.
package memory

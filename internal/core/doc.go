// Package core 定义跨包共享的接口与错误面——依赖倒置的接口提供方。
// Package core declares the interfaces and the error surface shared across
// packages; it is the provider side of dependency inversion.
//
// 本包只依赖标准库：不得 import 其他 internal 包。依赖方向
// channel/web/cli → core ← provider/memory/tool/storage/workspace 在此汇聚——
// 接口定义在消费方（core），实现放各自的包。
//
// This package depends on the standard library only: it must not import any
// other internal package. Interfaces live here because they are defined by the
// consumer side of the dependency-inversion edge.
package core

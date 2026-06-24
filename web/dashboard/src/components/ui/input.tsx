import * as React from "react"

import { cn } from "@/lib/utils"

function Input({
  className,
  type,
  // 浏览器（尤其 Chrome）会忽略 autoComplete="off" 对其记住的值做启发式自动填充，
  // 且会根据 name 属性猜测字段含义（如 name="email" 触发邮箱填充）。这里默认强制
  // 关闭自动填充，并显式传一个无意义 name，避免触发任何已知字段的填充。
  // 调用方仍可通过传入 autoComplete/name 覆盖（例如登录页用 new-password）。
  autoComplete = "off",
  name = "off",
  ...props
}: React.ComponentProps<"input">) {
  return (
    <input
      type={type}
      autoComplete={autoComplete}
      name={name}
      data-slot="input"
      className={cn(
        "h-8 w-full min-w-0 rounded-lg border border-input bg-transparent px-2.5 py-1 text-xs transition-colors outline-none file:inline-flex file:h-6 file:border-0 file:bg-transparent file:text-xs file:font-medium file:text-foreground placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-1 focus-visible:ring-ring/50 disabled:pointer-events-none disabled:cursor-not-allowed disabled:bg-input/50 disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-1 aria-invalid:ring-destructive/20 md:text-xs dark:bg-input/30 dark:disabled:bg-input/80 dark:aria-invalid:border-destructive/50 dark:aria-invalid:ring-destructive/40",
        className
      )}
      {...props}
    />
  )
}

export { Input }

import * as React from "react"

import { cn } from "@/lib/utils"

function Textarea({
  className,
  // 见 Input：Chrome 会忽略 autoComplete="off" 并按 name 猜测字段，故同样强制
  // 关闭自动填充并给一个无意义 name，避免被记住的值（如 API key）自动回填。
  autoComplete = "off",
  name = "off",
  ...props
}: React.ComponentProps<"textarea">) {
  return (
    <textarea
      autoComplete={autoComplete}
      name={name}
      data-slot="textarea"
      className={cn(
        "flex field-sizing-content min-h-16 w-full rounded-lg border border-input bg-transparent px-2.5 py-2 text-xs transition-colors outline-none placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-1 focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:bg-input/50 disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-1 aria-invalid:ring-destructive/20 md:text-xs dark:bg-input/30 dark:disabled:bg-input/80 dark:aria-invalid:border-destructive/50 dark:aria-invalid:ring-destructive/40",
        className
      )}
      {...props}
    />
  )
}

export { Textarea }

import React from 'react'
import { Card, Divider, Space, Stat } from '@ossrandom/design-system'
import type { StatProps } from '@ossrandom/design-system'

interface StatRowProps {
  items: readonly StatProps[]
}

const StatRow: React.FC<StatRowProps> = ({ items }) => {
  return (
    <Card bordered padding="md" radius="md">
      <Space size="lg" wrap>
        {items.map((item, index) => (
          <React.Fragment key={`${index}-${String(item.label)}`}>
            {index > 0 && <Divider direction="vertical" />}
            <Stat {...item} />
          </React.Fragment>
        ))}
      </Space>
    </Card>
  )
}

export default React.memo(StatRow)

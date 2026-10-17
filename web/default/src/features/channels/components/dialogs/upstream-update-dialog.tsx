/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { Search } from 'lucide-react'
import { useState, useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { ScrollArea } from '@/components/ui/scroll-area'

interface UpstreamUpdateDialogProps {
  open: boolean
  addModels: string[]
  confirmLoading: boolean
  onConfirm: (data: { addModels: string[] }) => void
  onCancel: () => void
}

export function UpstreamUpdateDialog(props: UpstreamUpdateDialogProps) {
  const { t } = useTranslation()
  const [searchAdd, setSearchAdd] = useState('')
  const [selectedAdd, setSelectedAdd] = useState<Set<string>>(
    () => new Set(props.addModels)
  )

  const filteredAdd = useMemo(
    () =>
      props.addModels.filter((m) =>
        m.toLowerCase().includes(searchAdd.toLowerCase())
      ),
    [props.addModels, searchAdd]
  )

  const toggleModel = (
    model: string,
    set: Set<string>,
    setter: (s: Set<string>) => void
  ) => {
    const next = new Set(set)
    if (next.has(model)) next.delete(model)
    else next.add(model)
    setter(next)
  }

  const toggleAllVisible = (
    models: string[],
    set: Set<string>,
    setter: (s: Set<string>) => void
  ) => {
    const allSelected = models.every((m) => set.has(m))
    const next = new Set(set)
    if (allSelected) {
      models.forEach((m) => next.delete(m))
    } else {
      models.forEach((m) => next.add(m))
    }
    setter(next)
  }

  return (
    <Dialog
      open={props.open}
      onOpenChange={(v) => !v && props.onCancel()}
      title={t('Upstream Model Updates')}
      contentClassName='sm:max-w-lg'
      contentHeight='auto'
      bodyClassName='space-y-4'
      footer={
        <>
          <Button variant='outline' onClick={props.onCancel}>
            {t('Cancel')}
          </Button>
          <Button
            onClick={() => props.onConfirm({ addModels: [...selectedAdd] })}
            disabled={props.confirmLoading || props.addModels.length === 0}
          >
            {t('Confirm')}
          </Button>
        </>
      }
    >
      <p className='text-muted-foreground text-sm'>
        {t(
          'Select models to process. Unselected "add" models will be ignored.'
        )}
      </p>
      <div className='relative'>
        <Search className='text-muted-foreground absolute top-2.5 left-2.5 h-4 w-4' />
        <Input
          placeholder={t('Search models...')}
          className='pl-8'
          value={searchAdd}
          onChange={(e) => setSearchAdd(e.target.value)}
        />
      </div>
      {filteredAdd.length > 0 && (
        <div className='flex items-center gap-2'>
          <Checkbox
            checked={filteredAdd.every((m) => selectedAdd.has(m))}
            onCheckedChange={() =>
              toggleAllVisible(filteredAdd, selectedAdd, setSelectedAdd)
            }
          />
          <span className='text-muted-foreground text-xs'>
            {t('Select All Visible')}
          </span>
        </div>
      )}
      <ScrollArea className='h-[280px] rounded-md border p-2'>
        {filteredAdd.length > 0 ? (
          <div className='space-y-1'>
            {filteredAdd.map((model) => (
              <label
                key={model}
                className='hover:bg-accent flex cursor-pointer items-center gap-2 rounded px-2 py-1.5'
              >
                <Checkbox
                  checked={selectedAdd.has(model)}
                  onCheckedChange={() =>
                    toggleModel(model, selectedAdd, setSelectedAdd)
                  }
                />
                <span className='truncate text-sm'>{model}</span>
              </label>
            ))}
          </div>
        ) : (
          <p className='text-muted-foreground py-8 text-center text-sm'>
            {props.addModels.length === 0
              ? t('No models to add')
              : t('No matching results')}
          </p>
        )}
      </ScrollArea>
    </Dialog>
  )
}

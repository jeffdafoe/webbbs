<?php

namespace App\Door\ZChat\Entity\Enum;

enum MessageType: string
{
    case NORMAL = 'normal';
    case WHISPER = 'whisper';
    case ACTION = 'action';
    case SYSTEM = 'system';
    case GAME = 'game';
}
